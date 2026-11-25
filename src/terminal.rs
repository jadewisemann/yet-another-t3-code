//! The terminal panel: one shell per thread, run by the server (`terminal.attach`), drawn
//! from an alacritty_terminal grid. Keys go to the shell as bytes; the server's output
//! goes through the emulator.

use std::cell::{Cell, RefCell};
use std::rc::Rc;

use alacritty_terminal::event::{Event, EventListener};
use alacritty_terminal::grid::{Dimensions, Scroll};
use alacritty_terminal::term::cell::Flags;
use alacritty_terminal::term::test::TermSize;
use alacritty_terminal::term::{Config, Term, TermMode};
use alacritty_terminal::vte::ansi::{Color, NamedColor, Processor, Rgb};
use gpui::{
    App, Bounds, Context, FocusHandle, Focusable, FontWeight, Hsla, KeyDownEvent, MouseButton,
    Pixels, ScrollWheelEvent, Window, canvas, div, font, prelude::*, px,
};
use serde_json::{Value, json};

use crate::protocol::method;
use crate::rpc::RpcHandle;
use crate::theme::{self, Palette, Theme};

/// The terminal a thread's panel shows. Upstream's web app numbers them the same way.
pub const TERMINAL_ID: &str = "term-1";

/// Collects the answers the emulator wants to send back to the shell.
#[derive(Clone, Default)]
struct Replies(Rc<RefCell<Vec<String>>>);

impl EventListener for Replies {
    fn send_event(&self, event: Event) {
        if let Event::PtyWrite(text) = event {
            self.0.borrow_mut().push(text);
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
enum ShellState {
    Starting,
    Running,
    Exited(String),
    Failed(String),
}

pub struct TerminalPanel {
    focus_handle: FocusHandle,
    rpc: RpcHandle,
    pub thread_id: String,
    pub key: String,
    cwd: String,
    term: Term<Replies>,
    replies: Replies,
    parser: Processor,
    cols: usize,
    rows: usize,
    state: ShellState,
    /// A write is on its way; later input waits in `queued` so keys arrive in order.
    in_flight: bool,
    queued: String,
    bounds: Rc<Cell<Option<Bounds<Pixels>>>>,
}

fn new_term(cols: usize, rows: usize, replies: Replies) -> Term<Replies> {
    Term::new(Config::default(), &TermSize::new(cols, rows), replies)
}

impl TerminalPanel {
    pub fn new(rpc: RpcHandle, thread_id: String, cwd: String, cx: &mut Context<Self>) -> Self {
        let replies = Replies::default();
        let (cols, rows) = (80, 12);
        let key = format!("terminal:{thread_id}:{}", uuid::Uuid::new_v4());
        let panel = Self {
            focus_handle: cx.focus_handle(),
            rpc,
            thread_id,
            key,
            cwd,
            term: new_term(cols, rows, replies.clone()),
            replies,
            parser: Processor::new(),
            cols,
            rows,
            state: ShellState::Starting,
            in_flight: false,
            queued: String::new(),
            bounds: Rc::default(),
        };
        panel.attach();
        panel
    }

    fn target(&self) -> Value {
        json!({ "threadId": self.thread_id, "terminalId": TERMINAL_ID })
    }

    fn attach(&self) {
        let mut payload = self.target();
        payload["cwd"] = json!(self.cwd);
        payload["cols"] = json!(self.cols);
        payload["rows"] = json!(self.rows);
        self.rpc.attach_terminal(Some((self.key.clone(), payload)));
    }

    pub fn detach(&self) {
        self.rpc.attach_terminal(None);
    }

    /// A value from the terminal stream.
    pub fn on_event(&mut self, value: Value, cx: &mut Context<Self>) {
        match value.get("type").and_then(Value::as_str).unwrap_or("") {
            "snapshot" | "restarted" => {
                let snapshot = value.get("snapshot").cloned().unwrap_or(Value::Null);
                self.term = new_term(self.cols, self.rows, self.replies.clone());
                self.parser = Processor::new();
                if let Some(history) = snapshot.get("history").and_then(Value::as_str) {
                    self.feed(history);
                }
                // History replays old answers' questions; only live output gets answers.
                self.replies.0.borrow_mut().clear();
                self.state = match snapshot.get("status").and_then(Value::as_str) {
                    Some("running") => ShellState::Running,
                    Some("starting") => ShellState::Starting,
                    Some("error") => ShellState::Failed(
                        snapshot
                            .get("error")
                            .and_then(Value::as_str)
                            .unwrap_or("the shell did not start")
                            .to_string(),
                    ),
                    _ => ShellState::Exited(exit_text(&snapshot)),
                };
                let cols = snapshot.get("cols").and_then(Value::as_u64);
                let rows = snapshot.get("rows").and_then(Value::as_u64);
                if cols != Some(self.cols as u64) || rows != Some(self.rows as u64) {
                    self.send_resize();
                }
            }
            "output" => {
                if let Some(data) = value.get("data").and_then(Value::as_str) {
                    self.feed(data);
                    let replies: Vec<String> = self.replies.0.borrow_mut().drain(..).collect();
                    for reply in replies {
                        self.write(&reply);
                    }
                }
            }
            "cleared" => self.feed("\x1b[H\x1b[2J\x1b[3J"),
            "exited" => self.state = ShellState::Exited(exit_text(&value)),
            "error" => {
                let message = value
                    .get("message")
                    .and_then(Value::as_str)
                    .unwrap_or("error");
                self.state = ShellState::Failed(message.to_string());
            }
            "closed" => self.state = ShellState::Exited("The terminal was closed.".into()),
            _ => {}
        }
        cx.notify();
    }

    pub fn on_ended(&mut self, error: Option<String>, cx: &mut Context<Self>) {
        if let Some(error) = error {
            self.state = ShellState::Failed(error);
        }
        cx.notify();
    }

    pub fn on_write_done(&mut self) {
        self.in_flight = false;
        if !self.queued.is_empty() {
            let data = std::mem::take(&mut self.queued);
            self.write(&data);
        }
    }

    fn feed(&mut self, data: &str) {
        self.parser.advance(&mut self.term, data.as_bytes());
    }

    fn write(&mut self, data: &str) {
        if data.is_empty() {
            return;
        }
        if self.in_flight {
            self.queued.push_str(data);
            return;
        }
        self.in_flight = true;
        let mut payload = self.target();
        payload["data"] = json!(data);
        self.rpc
            .query(self.key.clone(), method::TERMINAL_WRITE, payload);
    }

    fn send_resize(&self) {
        let mut payload = self.target();
        payload["cols"] = json!(self.cols);
        payload["rows"] = json!(self.rows);
        self.rpc
            .query("terminal-resize", method::TERMINAL_RESIZE, payload);
    }

    fn restart(&mut self) {
        let mut payload = self.target();
        payload["cwd"] = json!(self.cwd);
        payload["cols"] = json!(self.cols);
        payload["rows"] = json!(self.rows);
        self.state = ShellState::Starting;
        self.rpc
            .query("terminal-open", method::TERMINAL_OPEN, payload);
    }

    pub fn clear(&mut self, cx: &mut Context<Self>) {
        self.rpc
            .query("terminal-clear", method::TERMINAL_CLEAR, self.target());
        cx.notify();
    }

    fn on_key_down(&mut self, event: &KeyDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        let keystroke = &event.keystroke;
        let mods = keystroke.modifiers;
        // Command shortcuts stay with the app on macOS.
        if mods.platform {
            if keystroke.key == "v"
                && let Some(text) = cx.read_from_clipboard().and_then(|item| item.text())
            {
                self.paste(&text);
                cx.stop_propagation();
            } else if keystroke.key == "k" {
                self.clear(cx);
                cx.stop_propagation();
            }
            return;
        }
        if !matches!(self.state, ShellState::Running) {
            if keystroke.key == "enter" {
                self.restart();
                cx.stop_propagation();
                cx.notify();
            }
            return;
        }
        // Linux terminals paste with ctrl-shift-v.
        if mods.control && mods.shift && keystroke.key == "v" {
            if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
                self.paste(&text);
            }
            cx.stop_propagation();
            return;
        }
        let app_cursor = self.term.mode().contains(TermMode::APP_CURSOR);
        if let Some(bytes) = key_bytes(
            &keystroke.key,
            keystroke.key_char.as_deref(),
            mods.control,
            mods.alt,
            mods.shift,
            app_cursor,
        ) {
            self.term.scroll_display(Scroll::Bottom);
            self.write(&bytes);
            cx.stop_propagation();
            cx.notify();
        }
    }

    fn paste(&mut self, text: &str) {
        let text = text.replace("\r\n", "\r").replace('\n', "\r");
        if self.term.mode().contains(TermMode::BRACKETED_PASTE) {
            let clean = text.replace('\x1b', "");
            self.write(&format!("\x1b[200~{clean}\x1b[201~"));
        } else {
            self.write(&text);
        }
    }

    fn on_scroll(&mut self, event: &ScrollWheelEvent, _: &mut Window, cx: &mut Context<Self>) {
        let line = px(cx.global::<Theme>().font_size * 1.3);
        let delta = event.delta.pixel_delta(line).y;
        let lines = (f32::from(delta) / f32::from(line)).round() as i32;
        if lines != 0 {
            self.term.scroll_display(Scroll::Delta(lines));
            cx.notify();
        }
    }

    /// Fits the grid to the panel's last measured size.
    fn fit(&mut self, cell_width: Pixels, line_height: Pixels) {
        let Some(bounds) = self.bounds.get() else {
            return;
        };
        let cols = ((f32::from(bounds.size.width) / f32::from(cell_width)).floor() as usize)
            .clamp(20, 1000);
        let rows = ((f32::from(bounds.size.height) / f32::from(line_height)).floor() as usize)
            .clamp(3, 1000);
        if cols != self.cols || rows != self.rows {
            self.cols = cols;
            self.rows = rows;
            self.term.resize(TermSize::new(cols, rows));
            self.send_resize();
        }
    }
}

fn exit_text(value: &Value) -> String {
    let signal = value.get("exitSignal").and_then(Value::as_i64).unwrap_or(0);
    match value.get("exitCode").and_then(Value::as_i64) {
        _ if signal != 0 => format!("The shell was stopped by signal {signal}."),
        Some(code) => format!("The shell exited with code {code}."),
        None => "The shell is not running.".into(),
    }
}

/// The bytes a key sends to the shell, as xterm sends them.
pub fn key_bytes(
    key: &str,
    key_char: Option<&str>,
    control: bool,
    alt: bool,
    shift: bool,
    app_cursor: bool,
) -> Option<String> {
    let csi = |code: &str, normal: &str| {
        if app_cursor && !control && !alt && !shift {
            format!("\x1bO{code}")
        } else {
            let modifier = 1 + shift as u8 + 2 * alt as u8 + 4 * control as u8;
            if modifier > 1 {
                format!("\x1b[1;{modifier}{code}")
            } else {
                normal.to_string()
            }
        }
    };
    let special = match key {
        "enter" => Some("\r".to_string()),
        "tab" if shift => Some("\x1b[Z".to_string()),
        "tab" => Some("\t".to_string()),
        "backspace" if alt => Some("\x1b\x7f".to_string()),
        "backspace" => Some("\x7f".to_string()),
        "escape" => Some("\x1b".to_string()),
        "up" => Some(csi("A", "\x1b[A")),
        "down" => Some(csi("B", "\x1b[B")),
        "right" => Some(csi("C", "\x1b[C")),
        "left" => Some(csi("D", "\x1b[D")),
        "home" => Some(csi("H", "\x1b[H")),
        "end" => Some(csi("F", "\x1b[F")),
        "delete" => Some("\x1b[3~".to_string()),
        "pageup" => Some("\x1b[5~".to_string()),
        "pagedown" => Some("\x1b[6~".to_string()),
        "insert" => Some("\x1b[2~".to_string()),
        "f1" => Some("\x1bOP".to_string()),
        "f2" => Some("\x1bOQ".to_string()),
        "f3" => Some("\x1bOR".to_string()),
        "f4" => Some("\x1bOS".to_string()),
        "space" if control => Some("\0".to_string()),
        _ => None,
    };
    if special.is_some() {
        return special;
    }
    if control {
        let c = key.chars().next().filter(|_| key.chars().count() == 1)?;
        let byte = match c.to_ascii_lowercase() {
            c @ 'a'..='z' => c as u8 - b'a' + 1,
            '@' | '2' => 0,
            '[' | '3' => 0x1b,
            '\\' | '4' => 0x1c,
            ']' | '5' => 0x1d,
            '^' | '6' => 0x1e,
            '_' | '-' | '7' => 0x1f,
            '/' => 0x1f,
            _ => return None,
        };
        let text = (byte as char).to_string();
        return Some(if alt { format!("\x1b{text}") } else { text });
    }
    let text = match key_char {
        Some(text) if !text.is_empty() => text.to_string(),
        _ if key == "space" => " ".to_string(),
        _ if key.chars().count() == 1 => {
            if shift {
                key.to_uppercase()
            } else {
                key.to_string()
            }
        }
        _ => return None,
    };
    // Option types special characters on macOS (key_char has them); elsewhere alt is meta.
    if alt && !cfg!(target_os = "macos") {
        return Some(format!("\x1b{text}"));
    }
    Some(text)
}

const DARK_ANSI: [u32; 16] = [
    0x000000, 0xcd3131, 0x0dbc79, 0xe5e510, 0x2472c8, 0xbc3fbc, 0x11a8cd, 0xe5e5e5, 0x666666,
    0xf14c4c, 0x23d18b, 0xf5f543, 0x3b8eea, 0xd670d6, 0x29b8db, 0xffffff,
];
const LIGHT_ANSI: [u32; 16] = [
    0x000000, 0xcd3131, 0x00bc00, 0x949800, 0x0451a5, 0xbc05bc, 0x0598bc, 0x555555, 0x666666,
    0xcd3131, 0x14ce14, 0xb5ba00, 0x0451a5, 0xbc05bc, 0x0598bc, 0xa5a5a5,
];

struct Colors<'a> {
    term: &'a alacritty_terminal::term::color::Colors,
    ansi: &'static [u32; 16],
    fg: Hsla,
    bg: Hsla,
}

fn rgb(c: Rgb) -> Hsla {
    gpui::rgb(((c.r as u32) << 16) | ((c.g as u32) << 8) | c.b as u32).into()
}

impl Colors<'_> {
    fn indexed(&self, index: usize) -> Hsla {
        if let Some(c) = self.term[index] {
            return rgb(c);
        }
        let value = match index {
            0..=15 => self.ansi[index],
            16..=231 => {
                let i = index - 16;
                let level = |v: usize| if v == 0 { 0 } else { 55 + v as u32 * 40 };
                (level(i / 36) << 16) | (level((i / 6) % 6) << 8) | level(i % 6)
            }
            232..=255 => {
                let v = 8 + (index as u32 - 232) * 10;
                (v << 16) | (v << 8) | v
            }
            _ => return self.fg,
        };
        gpui::rgb(value).into()
    }

    fn resolve(&self, color: Color, dim: bool) -> Hsla {
        let mut out = match color {
            Color::Spec(c) => rgb(c),
            Color::Indexed(i) => self.indexed(i as usize),
            Color::Named(named) => match named {
                NamedColor::Foreground | NamedColor::BrightForeground => self.term
                    [NamedColor::Foreground as usize]
                    .map(rgb)
                    .unwrap_or(self.fg),
                NamedColor::Background => self.term[NamedColor::Background as usize]
                    .map(rgb)
                    .unwrap_or(self.bg),
                NamedColor::Cursor => self.fg,
                NamedColor::DimForeground => self.fg.opacity(0.7),
                other => {
                    let n = other as usize;
                    if (NamedColor::DimBlack as usize..=NamedColor::DimWhite as usize).contains(&n)
                    {
                        self.indexed(n - NamedColor::DimBlack as usize).opacity(0.7)
                    } else {
                        self.indexed(n.min(15))
                    }
                }
            },
        };
        if dim {
            out = out.opacity(0.7);
        }
        out
    }
}

#[derive(Clone, PartialEq)]
struct Style {
    fg: Hsla,
    bg: Option<Hsla>,
    bold: bool,
    italic: bool,
    underline: bool,
}

impl Render for TerminalPanel {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let p: Palette = theme::palette(window, cx);
        let theme = cx.global::<Theme>();
        let font_size = px(theme.font_size);
        let family = theme
            .code_font
            .clone()
            .unwrap_or_else(|| "monospace".into());
        let line_height = px((theme.font_size * 1.3).round());
        let font_id = window.text_system().resolve_font(&font(family.clone()));
        let cell_width = window
            .text_system()
            .advance(font_id, font_size, 'm')
            .map(|size| size.width)
            .unwrap_or(px(theme.font_size * 0.6));
        self.fit(cell_width, line_height);

        let focused = self.focus_handle.is_focused(window);
        let bg = p.surface;
        let colors = Colors {
            term: self.term.colors(),
            ansi: if bg.l > 0.5 { &LIGHT_ANSI } else { &DARK_ANSI },
            fg: p.text,
            bg,
        };
        let content = self.term.renderable_content();
        let offset = content.display_offset as i32;
        let cursor = content.cursor.point;
        let show_cursor = focused
            && offset == 0
            && content.mode.contains(TermMode::SHOW_CURSOR)
            && matches!(self.state, ShellState::Running);
        let rows = self.term.screen_lines();
        let mut lines: Vec<Vec<(Style, String)>> = vec![Vec::new(); rows];
        for indexed in content.display_iter {
            let row = indexed.point.line.0 + offset;
            if row < 0 || row as usize >= rows {
                continue;
            }
            let cell = &indexed.cell;
            if cell
                .flags
                .intersects(Flags::WIDE_CHAR_SPACER | Flags::LEADING_WIDE_CHAR_SPACER)
            {
                continue;
            }
            let dim = cell.flags.contains(Flags::DIM);
            let mut fg = colors.resolve(cell.fg, dim);
            let mut cell_bg = match cell.bg {
                Color::Named(NamedColor::Background) => None,
                other => Some(colors.resolve(other, false)),
            };
            let at_cursor = show_cursor && indexed.point == cursor;
            if cell.flags.contains(Flags::INVERSE) ^ at_cursor {
                let back = cell_bg.unwrap_or(bg);
                cell_bg = Some(fg);
                fg = back;
            }
            let c = if cell.flags.contains(Flags::HIDDEN) || cell.c == '\0' {
                ' '
            } else {
                cell.c
            };
            let style = Style {
                fg,
                bg: cell_bg,
                bold: cell.flags.contains(Flags::BOLD),
                italic: cell.flags.contains(Flags::ITALIC),
                underline: cell.flags.contains(Flags::UNDERLINE),
            };
            let line = &mut lines[row as usize];
            match line.last_mut() {
                Some((last, text)) if *last == style => text.push(c),
                _ => line.push((style, c.to_string())),
            }
        }

        let banner = match &self.state {
            ShellState::Running => None,
            ShellState::Starting => Some("Starting the shell…".to_string()),
            ShellState::Exited(text) => Some(format!("{text} Press Enter to start a new shell.")),
            ShellState::Failed(text) => Some(format!("{text} Press Enter to try again.")),
        };
        let bounds = self.bounds.clone();
        let grid = div()
            .relative()
            .flex_1()
            .overflow_hidden()
            .font_family(family)
            .text_size(font_size)
            .line_height(line_height)
            .child(
                canvas(
                    move |b, window, _| {
                        if bounds.get() != Some(b) {
                            bounds.set(Some(b));
                            window.refresh();
                        }
                    },
                    |_, _, _, _| {},
                )
                .absolute()
                .size_full(),
            )
            .children(lines.into_iter().map(|runs| {
                div()
                    .h(line_height)
                    .flex()
                    .whitespace_nowrap()
                    .children(runs.into_iter().map(|(style, text)| {
                        div()
                            .text_color(style.fg)
                            .when_some(style.bg, |el, bg| el.bg(bg))
                            .when(style.bold, |el| el.font_weight(FontWeight::BOLD))
                            .when(style.italic, |el| el.italic())
                            .when(style.underline, |el| el.underline())
                            .child(text)
                    }))
            }));
        div()
            .id("terminal")
            .key_context("Terminal")
            .track_focus(&self.focus_handle)
            .on_key_down(cx.listener(Self::on_key_down))
            .on_scroll_wheel(cx.listener(Self::on_scroll))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, _, window, _| window.focus(&this.focus_handle)),
            )
            .size_full()
            .flex()
            .flex_col()
            .bg(bg)
            .px_2()
            .py_1()
            .child(grid)
            .when_some(banner, |el, banner| {
                el.child(div().text_xs().text_color(p.muted).child(banner))
            })
    }
}

impl Focusable for TerminalPanel {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maps_keys_to_bytes() {
        assert_eq!(
            key_bytes("a", Some("a"), false, false, false, false).as_deref(),
            Some("a")
        );
        assert_eq!(
            key_bytes("c", None, true, false, false, false).as_deref(),
            Some("\x03")
        );
        assert_eq!(
            key_bytes("enter", None, false, false, false, false).as_deref(),
            Some("\r")
        );
        assert_eq!(
            key_bytes("up", None, false, false, false, false).as_deref(),
            Some("\x1b[A")
        );
        assert_eq!(
            key_bytes("up", None, false, false, false, true).as_deref(),
            Some("\x1bOA")
        );
        assert_eq!(
            key_bytes("left", None, true, false, false, false).as_deref(),
            Some("\x1b[1;5D")
        );
        assert_eq!(key_bytes("shift", None, false, false, true, false), None);
    }

    #[test]
    fn replays_output_into_the_grid() {
        let replies = Replies::default();
        let mut term = new_term(20, 3, replies.clone());
        let mut parser: Processor = Processor::new();
        parser.advance(&mut term, b"hi\r\n\x1b[31mred\x1b[0m\x1b[6n");
        let line: String = (0..2)
            .map(|c| {
                term.grid()[alacritty_terminal::index::Line(0)]
                    [alacritty_terminal::index::Column(c)]
                .c
            })
            .collect();
        assert_eq!(line, "hi");
        // The cursor report query gets an answer for the shell.
        assert_eq!(replies.0.borrow().len(), 1);
    }
}
