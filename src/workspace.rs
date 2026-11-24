//! The main window: sidebar of projects and threads, the open conversation, and the composer.

use std::collections::HashMap;

use gpui::{
    AnyElement, App, ClickEvent, Context, Entity, FocusHandle, Focusable, FontWeight, Hsla,
    ScrollHandle, SharedString, Subscription, Task, Window, div, prelude::*, px,
};

use crate::config::{self, Connection};
use crate::keymap::*;
use crate::local;
use crate::protocol::{
    self, ConversationMessage, ProjectShell, ShellStreamItem, ThreadShell, ThreadStreamItem,
};
use crate::rpc::{self, RpcEvent, RpcHandle};
use crate::text_input::{InputEvent, TextInput};
use crate::theme::{self, Palette, Theme};

#[derive(Clone, Debug, PartialEq)]
enum Status {
    NeedsPairing(Option<String>),
    Pairing,
    /// Looking for T3 Code on this machine and pairing with it.
    PairingLocal,
    Connecting,
    Connected,
    Disconnected(String),
}

pub struct Workspace {
    focus_handle: FocusHandle,
    status: Status,
    rpc: Option<RpcHandle>,
    projects: Vec<ProjectShell>,
    threads: HashMap<String, ThreadShell>,
    shell_synced: bool,
    selected_thread: Option<String>,
    /// Set while composing the first message of a new thread in this project.
    draft_project: Option<String>,
    messages: Vec<ConversationMessage>,
    messages_loaded: bool,
    composer: Entity<TextInput>,
    rename_input: Entity<TextInput>,
    renaming: bool,
    pair_input: Entity<TextInput>,
    /// The current server is on this machine, so a rejected token can be replaced silently.
    server_is_local: bool,
    /// Pairing again after a rejected token happens once per launch, so a server that keeps
    /// refusing (an incompatible protocol, say) does not loop.
    repaired_once: bool,
    sidebar_visible: bool,
    shortcuts_visible: bool,
    notice: Option<String>,
    scroll: ScrollHandle,
    _rpc_task: Option<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl Workspace {
    pub fn new(config_notice: Option<String>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let composer = cx.new(|cx| TextInput::new("Message the agent…", cx));
        let rename_input = cx.new(|cx| TextInput::new("Thread name", cx));
        let pair_input = cx.new(|cx| TextInput::new("http://127.0.0.1:3773/pair#token=…", cx));

        let subscriptions = vec![
            cx.subscribe_in(
                &composer,
                window,
                |this, _, event: &InputEvent, window, cx| {
                    if let InputEvent::Submit = event {
                        this.send_composer(window, cx);
                    }
                },
            ),
            cx.subscribe_in(
                &rename_input,
                window,
                |this, _, event: &InputEvent, window, cx| match event {
                    InputEvent::Submit => this.commit_rename(window, cx),
                    InputEvent::Cancel => this.cancel_rename(window, cx),
                    InputEvent::Changed => {}
                },
            ),
            cx.subscribe_in(
                &pair_input,
                window,
                |this, _, event: &InputEvent, window, cx| {
                    if let InputEvent::Submit = event {
                        this.pair(window, cx);
                    }
                },
            ),
            cx.observe_window_appearance(window, |_, _, cx| cx.notify()),
        ];

        let mut this = Self {
            focus_handle: cx.focus_handle(),
            status: Status::NeedsPairing(None),
            rpc: None,
            projects: Vec::new(),
            threads: HashMap::new(),
            shell_synced: false,
            selected_thread: None,
            draft_project: None,
            messages: Vec::new(),
            messages_loaded: false,
            composer,
            rename_input,
            renaming: false,
            pair_input,
            server_is_local: false,
            repaired_once: false,
            sidebar_visible: true,
            shortcuts_visible: false,
            notice: config_notice,
            scroll: ScrollHandle::new(),
            _rpc_task: None,
            _subscriptions: subscriptions,
        };
        match config::load_connection() {
            Some(connection) => this.start_server_and_connect(connection, window, cx),
            None => {
                window.focus(&this.pair_input.focus_handle(cx));
                this.pair_local(window, cx);
            }
        }
        this
    }

    fn connect(&mut self, connection: Connection, window: &mut Window, cx: &mut Context<Self>) {
        let (tx, rx) = async_channel::unbounded();
        self.server_is_local = local::is_local(&connection);
        self.rpc = Some(rpc::start(connection, tx));
        self.status = Status::Connecting;
        self._rpc_task = Some(cx.spawn_in(window, async move |this, cx| {
            while let Ok(event) = rx.recv().await {
                let alive = this
                    .update_in(cx, |this, window, cx| this.on_rpc_event(event, window, cx))
                    .is_ok();
                if !alive {
                    break;
                }
            }
        }));
        window.focus(&self.composer.focus_handle(cx));
        cx.notify();
    }

    fn on_rpc_event(&mut self, event: RpcEvent, window: &mut Window, cx: &mut Context<Self>) {
        match event {
            RpcEvent::Connecting => {
                if self.status != Status::Connected {
                    self.status = Status::Connecting;
                }
            }
            RpcEvent::Connected => self.status = Status::Connected,
            RpcEvent::Disconnected(reason) => {
                self.status = Status::Disconnected(reason);
                self.shell_synced = false;
            }
            RpcEvent::Unauthorized(reason) => {
                self.rpc = None;
                self.status = Status::NeedsPairing(Some(format!(
                    "The server did not accept the saved token ({reason}). Pair again."
                )));
                window.focus(&self.pair_input.focus_handle(cx));
                // An expired token for a server on this machine can be replaced without asking.
                if self.server_is_local
                    && !self.repaired_once
                    && std::env::var_os("T3_TOKEN").is_none()
                {
                    self.repaired_once = true;
                    self.pair_local(window, cx);
                }
            }
            RpcEvent::Shell(item) => self.on_shell_item(item),
            RpcEvent::Thread { thread_id, item } => {
                if self.selected_thread.as_deref() == Some(thread_id.as_str()) {
                    self.on_thread_item(item);
                }
            }
            RpcEvent::CommandFailed { label, error } => {
                self.notice = Some(format!("{label} failed: {error}"));
            }
            RpcEvent::Launched { thread_id } => {
                if let Some(thread_id) = thread_id {
                    self.select_thread(Some(thread_id), window, cx);
                }
            }
        }
        cx.notify();
    }

    fn on_shell_item(&mut self, item: ShellStreamItem) {
        match item {
            ShellStreamItem::Snapshot { snapshot } => {
                self.projects = snapshot.projects;
                self.threads = snapshot
                    .threads
                    .into_iter()
                    .map(|thread| (thread.id.clone(), thread))
                    .collect();
                self.sort_projects();
            }
            ShellStreamItem::Synchronized => self.shell_synced = true,
            ShellStreamItem::ProjectUpdated { project } => {
                match self.projects.iter_mut().find(|p| p.id == project.id) {
                    Some(existing) => *existing = project,
                    None => self.projects.push(project),
                }
                self.sort_projects();
            }
            ShellStreamItem::ProjectRemoved { project_id } => {
                self.projects.retain(|p| p.id != project_id);
            }
            ShellStreamItem::ThreadUpdated { location, thread } => {
                if location == "active" {
                    self.threads.insert(thread.id.clone(), thread);
                } else {
                    self.threads.remove(&thread.id);
                }
            }
            ShellStreamItem::ThreadRemoved { thread_id } => {
                self.threads.remove(&thread_id);
            }
            ShellStreamItem::Unknown => {}
        }
    }

    fn sort_projects(&mut self) {
        self.projects.sort_by(|a, b| {
            b.pinned_at
                .is_some()
                .cmp(&a.pinned_at.is_some())
                .then_with(|| a.title.to_lowercase().cmp(&b.title.to_lowercase()))
        });
    }

    fn on_thread_item(&mut self, item: ThreadStreamItem) {
        match item {
            ThreadStreamItem::Snapshot { projection } => {
                self.messages = projection.messages;
                self.messages
                    .sort_by(|a, b| a.created_at.cmp(&b.created_at));
                self.messages_loaded = true;
                self.scroll_to_end();
            }
            ThreadStreamItem::Event { event } => {
                if let Some(message) = protocol::message_from_event(&event) {
                    match self.messages.iter_mut().find(|m| m.id == message.id) {
                        Some(existing) => *existing = message,
                        None => {
                            let at = self
                                .messages
                                .partition_point(|m| m.created_at <= message.created_at);
                            self.messages.insert(at, message);
                        }
                    }
                    self.scroll_to_end();
                }
            }
            ThreadStreamItem::Synchronized => self.messages_loaded = true,
            ThreadStreamItem::Unknown => {}
        }
    }

    fn scroll_to_end(&self) {
        let count = self.visible_messages().count();
        if count > 0 {
            self.scroll.scroll_to_item(count - 1);
        }
    }

    fn visible_messages(&self) -> impl Iterator<Item = &ConversationMessage> {
        self.messages
            .iter()
            .filter(|m| m.workflow_script != Some(true) && !(m.text.is_empty() && !m.streaming))
    }

    /// Projects with their listed threads, newest activity first, in sidebar order.
    fn sidebar_groups(&self) -> Vec<(&ProjectShell, Vec<&ThreadShell>)> {
        self.projects
            .iter()
            .filter(|p| p.archived_at.is_none())
            .map(|project| {
                let mut threads: Vec<&ThreadShell> = self
                    .threads
                    .values()
                    .filter(|t| t.project_id == project.id && t.is_listed())
                    .collect();
                threads.sort_by(|a, b| {
                    b.pinned_at
                        .is_some()
                        .cmp(&a.pinned_at.is_some())
                        .then_with(|| b.sort_key().cmp(a.sort_key()))
                });
                (project, threads)
            })
            .collect()
    }

    fn sidebar_order(&self) -> Vec<String> {
        self.sidebar_groups()
            .into_iter()
            .flat_map(|(_, threads)| threads.into_iter().map(|t| t.id.clone()))
            .collect()
    }

    fn current_thread(&self) -> Option<&ThreadShell> {
        self.selected_thread
            .as_ref()
            .and_then(|id| self.threads.get(id))
    }

    fn current_project_id(&self) -> Option<String> {
        self.draft_project
            .clone()
            .or_else(|| self.current_thread().map(|t| t.project_id.clone()))
            .or_else(|| {
                self.projects
                    .iter()
                    .find(|p| p.archived_at.is_none())
                    .map(|p| p.id.clone())
            })
    }

    fn select_thread(
        &mut self,
        thread_id: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.renaming {
            self.renaming = false;
        }
        if thread_id.is_some() {
            self.draft_project = None;
        }
        if self.selected_thread != thread_id {
            self.selected_thread = thread_id.clone();
            self.messages.clear();
            self.messages_loaded = false;
            if let Some(rpc) = &self.rpc {
                rpc.subscribe_thread(thread_id.clone());
            }
        }
        if let Some(thread_id) = &thread_id
            && let Some(rpc) = &self.rpc
        {
            rpc.dispatch("Mark visited", protocol::visit_thread(thread_id));
        }
        window.focus(&self.composer.focus_handle(cx));
        cx.notify();
    }

    fn send_composer(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let text = self.composer.read(cx).text().trim().to_string();
        if text.is_empty() {
            return;
        }
        let Some(rpc) = self.rpc.clone() else { return };
        if let Some(thread) = self.current_thread() {
            rpc.dispatch(
                "Send",
                protocol::send_message(&thread.id, &text, thread.is_working()),
            );
        } else if let Some(project_id) = self.current_project_id() {
            let project = self.projects.iter().find(|p| p.id == project_id);
            let recent = self
                .threads
                .values()
                .filter(|t| t.project_id == project_id && t.model_selection.is_some())
                .max_by(|a, b| a.sort_key().cmp(b.sort_key()))
                .or_else(|| {
                    self.threads
                        .values()
                        .max_by(|a, b| a.sort_key().cmp(b.sort_key()))
                });
            let Some(defaults) = rpc::launch_defaults_from(recent, project) else {
                self.notice = Some(
                    "No model to start with yet. Start one thread in T3 Code first, then this app reuses its model."
                        .into(),
                );
                cx.notify();
                return;
            };
            rpc.launch_thread(protocol::launch_thread(&project_id, &defaults, &text));
        } else {
            self.notice = Some("Add a project in T3 Code first.".into());
            cx.notify();
            return;
        }
        self.composer.update(cx, |input, cx| input.set_text("", cx));
        self.notice = None;
        window.focus(&self.composer.focus_handle(cx));
        cx.notify();
    }

    fn pair(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let link = self.pair_input.read(cx).text().trim().to_string();
        if link.is_empty() {
            return;
        }
        self.status = Status::Pairing;
        cx.notify();
        let task = cx
            .background_executor()
            .spawn(async move { config::pair(&link) });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update_in(cx, |this, window, cx| match result {
                Ok(connection) => {
                    this.pair_input
                        .update(cx, |input, cx| input.set_text("", cx));
                    this.connect(connection, window, cx);
                }
                Err(error) => {
                    this.status = Status::NeedsPairing(Some(format!("{error:#}")));
                    cx.notify();
                }
            })
            .ok();
        })
        .detach();
    }

    // Actions

    fn new_thread(&mut self, _: &NewThread, window: &mut Window, cx: &mut Context<Self>) {
        let project = self.current_project_id();
        self.select_thread(None, window, cx);
        self.draft_project = project;
        cx.notify();
    }

    fn new_thread_in(&mut self, project_id: String, window: &mut Window, cx: &mut Context<Self>) {
        self.select_thread(None, window, cx);
        self.draft_project = Some(project_id);
        cx.notify();
    }

    fn rename_thread(&mut self, _: &RenameThread, window: &mut Window, cx: &mut Context<Self>) {
        let Some(title) = self.current_thread().map(|t| t.title.clone()) else {
            return;
        };
        self.renaming = true;
        self.rename_input.update(cx, |input, cx| {
            input.set_text(title, cx);
            input.select_all_text(cx);
        });
        window.focus(&self.rename_input.focus_handle(cx));
        cx.notify();
    }

    fn commit_rename(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let title = self.rename_input.read(cx).text().trim().to_string();
        let thread_id = self.selected_thread.clone();
        self.renaming = false;
        if let (Some(thread_id), Some(rpc)) = (thread_id, self.rpc.clone())
            && !title.is_empty()
            && let Some(thread) = self.threads.get_mut(&thread_id)
            && thread.title != title
        {
            // Show the new name now; the server's thread.updated confirms it.
            thread.title = title.clone();
            rpc.dispatch("Rename", protocol::rename_thread(&thread_id, &title));
        }
        window.focus(&self.composer.focus_handle(cx));
        cx.notify();
    }

    fn cancel_rename(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.renaming = false;
        window.focus(&self.composer.focus_handle(cx));
        cx.notify();
    }

    fn step_thread(&mut self, delta: isize, window: &mut Window, cx: &mut Context<Self>) {
        let order = self.sidebar_order();
        if order.is_empty() {
            return;
        }
        let current = self
            .selected_thread
            .as_ref()
            .and_then(|id| order.iter().position(|t| t == id));
        let next = match current {
            Some(index) => (index as isize + delta).rem_euclid(order.len() as isize) as usize,
            None if delta > 0 => 0,
            None => order.len() - 1,
        };
        self.select_thread(Some(order[next].clone()), window, cx);
    }

    fn next_thread(&mut self, _: &NextThread, window: &mut Window, cx: &mut Context<Self>) {
        self.step_thread(1, window, cx);
    }

    fn previous_thread(&mut self, _: &PreviousThread, window: &mut Window, cx: &mut Context<Self>) {
        self.step_thread(-1, window, cx);
    }

    fn focus_composer(&mut self, _: &FocusComposer, window: &mut Window, cx: &mut Context<Self>) {
        window.focus(&self.composer.focus_handle(cx));
    }

    fn toggle_sidebar(&mut self, _: &ToggleSidebar, _: &mut Window, cx: &mut Context<Self>) {
        self.sidebar_visible = !self.sidebar_visible;
        cx.notify();
    }

    fn toggle_shortcuts(&mut self, _: &ToggleShortcuts, _: &mut Window, cx: &mut Context<Self>) {
        self.shortcuts_visible = !self.shortcuts_visible;
        cx.notify();
    }

    fn stop_thread(&mut self, _: &StopThread, _: &mut Window, cx: &mut Context<Self>) {
        if let (Some(thread), Some(rpc)) = (self.current_thread(), &self.rpc)
            && thread.is_working()
        {
            rpc.dispatch("Stop", protocol::stop_thread(&thread.id));
            self.notice = Some("Stopping…".into());
            cx.notify();
        }
    }

    fn archive_thread(&mut self, _: &ArchiveThread, window: &mut Window, cx: &mut Context<Self>) {
        let Some(thread_id) = self.selected_thread.clone() else {
            return;
        };
        let Some(rpc) = self.rpc.clone() else { return };
        let order = self.sidebar_order();
        let next = order
            .iter()
            .position(|id| *id == thread_id)
            .and_then(|i| {
                order
                    .get(i + 1)
                    .or_else(|| i.checked_sub(1).and_then(|j| order.get(j)))
            })
            .cloned();
        rpc.dispatch("Archive", protocol::archive_thread(&thread_id));
        self.threads.remove(&thread_id);
        self.select_thread(next, window, cx);
    }

    fn reload_config(&mut self, _: &ReloadConfig, window: &mut Window, cx: &mut Context<Self>) {
        let keymap_error = crate::keymap::load(cx);
        let theme = Theme::load();
        let theme_error = theme.load_error.clone();
        cx.set_global(theme);
        self.notice = Some(
            [keymap_error, theme_error]
                .into_iter()
                .flatten()
                .collect::<Vec<_>>()
                .join(" · "),
        )
        .filter(|n| !n.is_empty())
        .or(Some("Reloaded keymap.json and theme.json".into()));
        window.refresh();
        cx.notify();
    }

    fn open_config_folder(&mut self, _: &OpenConfigFolder, _: &mut Window, cx: &mut Context<Self>) {
        let dir = config::config_dir();
        let _ = std::fs::create_dir_all(&dir);
        cx.reveal_path(&dir);
    }

    /// Starts the bundled server first when the saved connection points at this machine.
    fn start_server_and_connect(
        &mut self,
        connection: Connection,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if !local::is_local(&connection) || std::env::var_os("T3_SERVER_URL").is_some() {
            self.connect(connection, window, cx);
            return;
        }
        self.status = Status::Connecting;
        cx.notify();
        let task = cx
            .background_executor()
            .spawn(async move { local::prepare(connection) });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update_in(cx, |this, window, cx| match result {
                Ok(connection) => this.connect(connection, window, cx),
                Err(error) => {
                    this.status = Status::NeedsPairing(Some(format!("{error:#}")));
                    cx.notify();
                }
            })
            .ok();
        })
        .detach();
    }

    /// Finds T3 Code on this machine (opening the app if needed) and pairs with it.
    fn pair_local(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if matches!(self.status, Status::Pairing | Status::PairingLocal) {
            return;
        }
        self.status = Status::PairingLocal;
        cx.notify();
        let task = cx.background_executor().spawn(async move { local::pair() });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update_in(cx, |this, window, cx| match result {
                Ok(connection) => this.connect(connection, window, cx),
                Err(error) => {
                    this.status = Status::NeedsPairing(Some(format!("{error:#}")));
                    cx.notify();
                }
            })
            .ok();
        })
        .detach();
    }

    fn disconnect(&mut self, _: &Disconnect, window: &mut Window, cx: &mut Context<Self>) {
        config::forget_connection();
        self.rpc = None;
        self._rpc_task = None;
        self.projects.clear();
        self.threads.clear();
        self.selected_thread = None;
        self.messages.clear();
        self.status = Status::NeedsPairing(None);
        window.focus(&self.pair_input.focus_handle(cx));
        cx.notify();
    }

    // Rendering

    fn render_pairing(
        &self,
        message: Option<&str>,
        p: Palette,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let busy = self.status == Status::Pairing;
        let finding = self.status == Status::PairingLocal;
        div()
            .size_full()
            .flex()
            .items_center()
            .justify_center()
            .bg(p.background)
            .child(
                div()
                    .w(px(520.))
                    .p_6()
                    .rounded_xl()
                    .border_1()
                    .border_color(p.border)
                    .bg(p.surface)
                    .flex()
                    .flex_col()
                    .gap_3()
                    .child(div().text_xl().font_weight(FontWeight::SEMIBOLD).child("Connect to T3 Code"))
                    .child(div().text_color(p.muted).child(
                        "YATT runs the T3 Code server on this computer by itself, so there is nothing else to install.",
                    ))
                    .child(
                        div().flex().items_center().gap_2().child(
                            button(
                                "pair-local",
                                if finding {
                                    "Starting T3 Code on this computer…".to_string()
                                } else {
                                    "Start T3 Code on this computer".to_string()
                                },
                                p,
                            )
                            .on_click(cx.listener(|this, _: &ClickEvent, window, cx| {
                                this.pair_local(window, cx)
                            })),
                        ),
                    )
                    .child(div().text_color(p.muted).child(
                        "For a server on another machine, run `t3 pair` there (or create a pairing link in its settings), then paste the link here and press Enter.",
                    ))
                    .child(input_box(self.pair_input.clone(), p))
                    .when(busy, |el| el.child(div().text_color(p.muted).child("Pairing…")))
                    .when_some(message.map(str::to_owned), |el, message| {
                        el.child(div().text_color(p.danger).child(message))
                    })
                    .child(
                        div()
                            .text_sm()
                            .text_color(p.muted)
                            .child(format!("Settings folder: {}", config::config_dir().display())),
                    ),
            )
            .into_any_element()
    }

    fn render_sidebar(&self, p: Palette, window: &Window, cx: &mut Context<Self>) -> AnyElement {
        let groups = self.sidebar_groups();
        let new_thread_keys = window.keystroke_text_for(&NewThread);
        let mut list = div()
            .id("sidebar-list")
            .flex_1()
            .overflow_y_scroll()
            .px_2()
            .pb_2()
            .flex()
            .flex_col()
            .gap_0p5();
        for (project, threads) in groups {
            let project_id = project.id.clone();
            list = list.child(
                div()
                    .id(SharedString::from(format!("project-{}", project.id)))
                    .mt_3()
                    .mb_1()
                    .px_2()
                    .flex()
                    .justify_between()
                    .items_center()
                    .text_xs()
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(p.muted)
                    .child(project.title.clone())
                    .child(
                        div()
                            .id(SharedString::from(format!("new-in-{}", project.id)))
                            .px_1()
                            .rounded_sm()
                            .cursor_pointer()
                            .hover(|s| s.bg(p.row_hover))
                            .child("+")
                            .on_click(cx.listener(move |this, _: &ClickEvent, window, cx| {
                                this.new_thread_in(project_id.clone(), window, cx)
                            })),
                    ),
            );
            for thread in threads {
                let selected = self.selected_thread.as_deref() == Some(thread.id.as_str());
                let thread_id = thread.id.clone();
                let dot: Option<Hsla> = if thread.needs_attention() {
                    Some(p.danger)
                } else if thread.is_working() {
                    Some(p.working)
                } else {
                    None
                };
                list = list.child(
                    div()
                        .id(SharedString::from(format!("thread-{}", thread.id)))
                        .px_2()
                        .py_1p5()
                        .rounded_md()
                        .flex()
                        .items_center()
                        .gap_2()
                        .cursor_pointer()
                        .when(selected, |el| el.bg(p.row_selected))
                        .when(!selected, |el| el.hover(|s| s.bg(p.row_hover)))
                        .child(
                            div()
                                .size(px(6.))
                                .rounded_full()
                                .when_some(dot, |el, color| el.bg(color)),
                        )
                        .child(
                            div()
                                .flex_1()
                                .overflow_hidden()
                                .whitespace_nowrap()
                                .text_ellipsis()
                                .child(if thread.title.is_empty() {
                                    "Untitled".to_string()
                                } else {
                                    thread.title.clone()
                                }),
                        )
                        .on_click(cx.listener(move |this, event: &ClickEvent, window, cx| {
                            this.select_thread(Some(thread_id.clone()), window, cx);
                            if event.click_count() >= 2 {
                                this.rename_thread(&RenameThread, window, cx);
                            }
                        })),
                );
            }
        }

        let status = match &self.status {
            Status::Connected if self.shell_synced => "Connected".to_string(),
            Status::Connected | Status::Connecting => "Connecting…".to_string(),
            Status::Disconnected(reason) => format!("Reconnecting ({reason})"),
            Status::Pairing | Status::PairingLocal => "Pairing…".to_string(),
            Status::NeedsPairing(_) => "Not paired".to_string(),
        };

        div()
            .w(px(272.))
            .h_full()
            .flex()
            .flex_col()
            .bg(p.sidebar)
            .border_r_1()
            .border_color(p.border)
            .child(
                div()
                    .px_4()
                    .pt_4()
                    .pb_2()
                    .flex()
                    .justify_between()
                    .items_center()
                    .child(div().font_weight(FontWeight::SEMIBOLD).child("T3 Code"))
                    .child(
                        div()
                            .id("new-thread")
                            .px_2()
                            .py_1()
                            .rounded_md()
                            .text_sm()
                            .cursor_pointer()
                            .border_1()
                            .border_color(p.border)
                            .hover(|s| s.bg(p.row_hover))
                            .child(format!("New  {new_thread_keys}"))
                            .on_click(cx.listener(|this, _: &ClickEvent, window, cx| {
                                this.new_thread(&NewThread, window, cx)
                            })),
                    ),
            )
            .child(list)
            .child(
                div()
                    .px_4()
                    .py_2()
                    .border_t_1()
                    .border_color(p.border)
                    .text_xs()
                    .text_color(p.muted)
                    .flex()
                    .justify_between()
                    .child(status)
                    .child(
                        div()
                            .id("shortcuts")
                            .cursor_pointer()
                            .child(format!(
                                "Shortcuts {}",
                                window.keystroke_text_for(&ToggleShortcuts)
                            ))
                            .on_click(cx.listener(|this, _: &ClickEvent, window, cx| {
                                this.toggle_shortcuts(&ToggleShortcuts, window, cx)
                            })),
                    ),
            )
            .into_any_element()
    }

    fn render_header(&self, p: Palette, window: &Window, cx: &mut Context<Self>) -> AnyElement {
        let thread = self.current_thread();
        let project_title = self
            .current_project_id()
            .and_then(|id| self.projects.iter().find(|p| p.id == id))
            .map(|p| p.title.clone())
            .unwrap_or_default();
        let title: AnyElement = if self.renaming {
            div()
                .w(px(420.))
                .px_2()
                .py_1()
                .rounded_md()
                .border_1()
                .border_color(p.accent)
                .bg(p.surface)
                .child(self.rename_input.clone())
                .into_any_element()
        } else {
            let text = match thread {
                Some(thread) if !thread.title.is_empty() => thread.title.clone(),
                Some(_) => "Untitled".into(),
                None => "New thread".into(),
            };
            div()
                .id("thread-title")
                .font_weight(FontWeight::SEMIBOLD)
                .overflow_hidden()
                .whitespace_nowrap()
                .text_ellipsis()
                .cursor_pointer()
                .child(text)
                .on_click(cx.listener(|this, event: &ClickEvent, window, cx| {
                    if event.click_count() >= 2 {
                        this.rename_thread(&RenameThread, window, cx);
                    }
                }))
                .into_any_element()
        };
        let working = thread.is_some_and(|t| t.is_working());
        let attention = thread.is_some_and(|t| t.needs_attention());
        div()
            .h(px(52.))
            .px_5()
            .flex()
            .items_center()
            .gap_3()
            .border_b_1()
            .border_color(p.border)
            .child(div().text_color(p.muted).text_sm().child(project_title))
            .child(div().text_color(p.muted).child("/"))
            .child(div().flex_1().overflow_hidden().child(title))
            .when(attention, |el| {
                el.child(chip("Needs your answer in T3 Code", p.danger, p))
            })
            .when(working, |el| el.child(chip("Working…", p.working, p)))
            .when(thread.is_some() && !self.renaming, |el| {
                el.child(
                    button(
                        "rename",
                        format!("Rename  {}", window.keystroke_text_for(&RenameThread)),
                        p,
                    )
                    .on_click(cx.listener(
                        |this, _: &ClickEvent, window, cx| {
                            this.rename_thread(&RenameThread, window, cx)
                        },
                    )),
                )
            })
            .when(working, |el| {
                el.child(
                    button(
                        "stop",
                        format!("Stop  {}", window.keystroke_text_for(&StopThread)),
                        p,
                    )
                    .on_click(cx.listener(
                        |this, _: &ClickEvent, window, cx| {
                            this.stop_thread(&StopThread, window, cx)
                        },
                    )),
                )
            })
            .into_any_element()
    }

    fn render_messages(&self, p: Palette, cx: &App) -> AnyElement {
        let message_font = cx.global::<Theme>().message_font.clone();
        let mut column = div()
            .id("messages")
            .flex_1()
            .overflow_y_scroll()
            .track_scroll(&self.scroll)
            .px_8()
            .py_6()
            .flex()
            .flex_col()
            .gap_5();
        if self.selected_thread.is_none() {
            return column
                .items_center()
                .justify_center()
                .child(
                    div()
                        .text_2xl()
                        .when_some(message_font.clone(), |el, font| el.font_family(font))
                        .child("What should we work on?"),
                )
                .child(
                    div()
                        .text_color(p.muted)
                        .child("Your first message starts a new thread in this project."),
                )
                .into_any_element();
        }
        if !self.messages_loaded {
            return column
                .items_center()
                .justify_center()
                .child(div().text_color(p.muted).child("Loading…"))
                .into_any_element();
        }
        for message in self.visible_messages() {
            let body = if message.text.is_empty() && message.streaming {
                "…".to_string()
            } else {
                message.text.clone()
            };
            let element = match message.role.as_str() {
                "user" => div().flex().justify_end().child(
                    div()
                        .max_w(px(640.))
                        .px_4()
                        .py_2p5()
                        .rounded_2xl()
                        .bg(p.user_bubble)
                        .child(body),
                ),
                "assistant" => div().child(
                    div()
                        .max_w(px(760.))
                        .line_height(gpui::relative(1.6))
                        .when_some(message_font.clone(), |el, font| el.font_family(font))
                        .child(body),
                ),
                _ => div().text_sm().text_color(p.muted).child(body),
            };
            column = column.child(element);
        }
        column.into_any_element()
    }

    fn render_composer(&self, p: Palette, window: &Window) -> AnyElement {
        let hint = format!(
            "Enter to send · {} new thread · {} rename · {} shortcuts",
            window.keystroke_text_for(&NewThread),
            window.keystroke_text_for(&RenameThread),
            window.keystroke_text_for(&ToggleShortcuts),
        );
        div()
            .px_8()
            .pb_5()
            .pt_2()
            .flex()
            .flex_col()
            .gap_1p5()
            .child(
                div()
                    .px_4()
                    .py_3()
                    .rounded_2xl()
                    .border_1()
                    .border_color(p.border)
                    .bg(p.surface)
                    .shadow_sm()
                    .child(self.composer.clone()),
            )
            .child(
                div()
                    .flex()
                    .justify_between()
                    .text_xs()
                    .text_color(p.muted)
                    .child(hint)
                    .when_some(self.notice.clone(), |el, notice| el.child(notice)),
            )
            .into_any_element()
    }

    fn render_shortcuts(&self, p: Palette, window: &Window) -> AnyElement {
        let mut rows = div().flex().flex_col().gap_1();
        for (label, action) in listed_actions() {
            let keys = window.keystroke_text_for(action.as_ref());
            rows = rows.child(div().flex().justify_between().gap_6().child(label).child(
                div().text_color(p.muted).child(if keys.is_empty() {
                    "unbound".to_string()
                } else {
                    keys
                }),
            ));
        }
        div()
            .absolute()
            .top(px(64.))
            .right(px(24.))
            .w(px(360.))
            .p_4()
            .rounded_xl()
            .border_1()
            .border_color(p.border)
            .bg(p.surface)
            .shadow_lg()
            .flex()
            .flex_col()
            .gap_3()
            .child(div().font_weight(FontWeight::SEMIBOLD).child("Shortcuts"))
            .child(rows)
            .child(div().text_xs().text_color(p.muted).child(format!(
                "Change them in {}",
                config::config_file("keymap.json").display()
            )))
            .into_any_element()
    }
}

fn input_box(input: Entity<TextInput>, p: Palette) -> impl IntoElement {
    div()
        .px_3()
        .py_2()
        .rounded_lg()
        .border_1()
        .border_color(p.border)
        .bg(p.background)
        .child(input)
}

fn chip(label: &'static str, color: Hsla, p: Palette) -> impl IntoElement {
    div()
        .px_2()
        .py_0p5()
        .rounded_full()
        .text_xs()
        .border_1()
        .border_color(color)
        .text_color(color)
        .bg(p.background)
        .child(label)
}

fn button(id: &'static str, label: String, p: Palette) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .px_2()
        .py_1()
        .rounded_md()
        .text_sm()
        .cursor_pointer()
        .border_1()
        .border_color(p.border)
        .hover(move |s| s.bg(p.row_hover))
        .child(label)
}

impl Focusable for Workspace {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for Workspace {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let p = theme::palette(window, cx);
        let theme = cx.global::<Theme>();
        let ui_font = theme.ui_font.clone();
        let font_size = theme.font_size;

        let root = div()
            .key_context("Workspace")
            .track_focus(&self.focus_handle)
            .on_action(cx.listener(Self::new_thread))
            .on_action(cx.listener(Self::rename_thread))
            .on_action(cx.listener(Self::next_thread))
            .on_action(cx.listener(Self::previous_thread))
            .on_action(cx.listener(Self::focus_composer))
            .on_action(cx.listener(Self::toggle_sidebar))
            .on_action(cx.listener(Self::toggle_shortcuts))
            .on_action(cx.listener(Self::stop_thread))
            .on_action(cx.listener(Self::archive_thread))
            .on_action(cx.listener(Self::reload_config))
            .on_action(cx.listener(Self::open_config_folder))
            .on_action(cx.listener(Self::disconnect))
            .size_full()
            .relative()
            .bg(p.background)
            .text_color(p.text)
            .text_size(px(font_size))
            .when_some(ui_font, |el, font| el.font_family(font));

        let pairing_message = match &self.status {
            Status::NeedsPairing(message) => Some(message.clone()),
            Status::Pairing | Status::PairingLocal => Some(None),
            _ => None,
        };
        if let Some(message) = pairing_message {
            return root.child(self.render_pairing(message.as_deref(), p, cx));
        }

        let main = div()
            .flex_1()
            .h_full()
            .flex()
            .flex_col()
            .child(self.render_header(p, window, cx))
            .child(self.render_messages(p, cx))
            .child(self.render_composer(p, window));

        root.child(
            div()
                .size_full()
                .flex()
                .when(self.sidebar_visible, |el| {
                    el.child(self.render_sidebar(p, window, cx))
                })
                .child(main),
        )
        .when(self.shortcuts_visible, |el| {
            el.child(self.render_shortcuts(p, window))
        })
    }
}
