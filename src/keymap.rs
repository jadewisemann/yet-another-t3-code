//! Key bindings. Defaults live here; `keymap.json` in the config folder adds to or replaces
//! them, in the same shape Zed uses:
//!
//! ```json
//! [
//!   { "context": "Workspace", "bindings": { "f2": "yatt::RenameThread", "cmd-n": null } }
//! ]
//! ```
//!
//! A binding to `null` removes that key. `secondary-` means cmd on macOS and ctrl elsewhere.
//! `yatt::ReloadConfig` re-reads the file without restarting.

use std::rc::Rc;

use anyhow::{Context as _, Result, anyhow};
use gpui::{
    Action, App, DummyKeyboardMapper, KeyBinding, KeyBindingContextPredicate, NoAction, actions,
};
use serde::Deserialize;

use crate::{config, text_input};

actions!(
    yatt,
    [
        /// Start a new thread in the current project.
        NewThread,
        /// Choose a folder to add as a project.
        AddProject,
        /// Rename the open thread.
        RenameThread,
        /// Switch the model a new thread starts on.
        CycleModel,
        /// Switch a new thread between the project folder and a new git worktree.
        ToggleWorktree,
        /// Open the next thread in the sidebar.
        NextThread,
        /// Open the previous thread in the sidebar.
        PreviousThread,
        /// Move focus to the message box.
        FocusComposer,
        /// Choose files to attach to the next message.
        AttachFiles,
        /// Show or hide the sidebar.
        ToggleSidebar,
        /// Show or hide the changes the open thread made.
        ToggleDiff,
        /// Switch the diff between all turns and the last turn.
        ToggleDiffScope,
        /// Show or hide the open thread's terminal.
        ToggleTerminal,
        /// Clear the terminal.
        ClearTerminal,
        /// Stop the running turn in the open thread.
        StopThread,
        /// Archive the open thread.
        ArchiveThread,
        /// Show the list of shortcuts.
        ToggleShortcuts,
        /// Re-read keymap.json and theme.json.
        ReloadConfig,
        /// Reveal the config folder.
        OpenConfigFolder,
        /// Forget the saved server and pair again.
        Disconnect,
        /// Quit the app.
        Quit,
    ]
);

/// Actions shown in the shortcut list, in order.
pub fn listed_actions() -> Vec<(&'static str, Box<dyn Action>)> {
    vec![
        ("New thread", Box::new(NewThread)),
        ("Add project", Box::new(AddProject)),
        ("Rename thread", Box::new(RenameThread)),
        ("Switch model for new threads", Box::new(CycleModel)),
        (
            "New thread in a worktree / the project folder",
            Box::new(ToggleWorktree),
        ),
        ("Next thread", Box::new(NextThread)),
        ("Previous thread", Box::new(PreviousThread)),
        ("Focus message box", Box::new(FocusComposer)),
        ("Attach files", Box::new(AttachFiles)),
        ("Toggle sidebar", Box::new(ToggleSidebar)),
        ("Show changes", Box::new(ToggleDiff)),
        ("Changes: all turns / last turn", Box::new(ToggleDiffScope)),
        ("Show terminal", Box::new(ToggleTerminal)),
        ("Clear terminal", Box::new(ClearTerminal)),
        ("Stop turn", Box::new(StopThread)),
        ("Archive thread", Box::new(ArchiveThread)),
        ("Show shortcuts", Box::new(ToggleShortcuts)),
        ("Reload config", Box::new(ReloadConfig)),
        ("Open config folder", Box::new(OpenConfigFolder)),
    ]
}

fn default_bindings() -> Vec<KeyBinding> {
    use text_input::*;
    let mut bindings = vec![
        // Text editing inside any input.
        KeyBinding::new("backspace", Backspace, Some("TextInput")),
        KeyBinding::new("delete", Delete, Some("TextInput")),
        KeyBinding::new("left", Left, Some("TextInput")),
        KeyBinding::new("right", Right, Some("TextInput")),
        KeyBinding::new("shift-left", SelectLeft, Some("TextInput")),
        KeyBinding::new("shift-right", SelectRight, Some("TextInput")),
        KeyBinding::new("alt-left", WordLeft, Some("TextInput")),
        KeyBinding::new("alt-right", WordRight, Some("TextInput")),
        KeyBinding::new("secondary-a", SelectAll, Some("TextInput")),
        KeyBinding::new("secondary-v", Paste, Some("TextInput")),
        KeyBinding::new("secondary-c", Copy, Some("TextInput")),
        KeyBinding::new("secondary-x", Cut, Some("TextInput")),
        KeyBinding::new("home", Home, Some("TextInput")),
        KeyBinding::new("end", End, Some("TextInput")),
        KeyBinding::new("secondary-left", Home, Some("TextInput")),
        KeyBinding::new("secondary-right", End, Some("TextInput")),
        KeyBinding::new("enter", Submit, Some("TextInput")),
        KeyBinding::new("escape", Cancel, Some("TextInput")),
        KeyBinding::new("ctrl-cmd-space", ShowCharacterPalette, Some("TextInput")),
        // Workspace commands. These also work while typing, because the inputs sit inside
        // the workspace and pass keys they do not handle up to it.
        // The last binding for an action is the one hints show, so F2 goes second.
        KeyBinding::new("secondary-shift-r", RenameThread, Some("Workspace")),
        KeyBinding::new("f2", RenameThread, Some("Workspace")),
        KeyBinding::new("secondary-n", NewThread, Some("Workspace")),
        KeyBinding::new("secondary-shift-o", AddProject, Some("Workspace")),
        KeyBinding::new("secondary-shift-m", CycleModel, Some("Workspace")),
        KeyBinding::new("secondary-alt-w", ToggleWorktree, Some("Workspace")),
        KeyBinding::new("alt-down", NextThread, Some("Workspace")),
        KeyBinding::new("alt-up", PreviousThread, Some("Workspace")),
        KeyBinding::new("secondary-]", NextThread, Some("Workspace")),
        KeyBinding::new("secondary-[", PreviousThread, Some("Workspace")),
        KeyBinding::new("secondary-l", FocusComposer, Some("Workspace")),
        KeyBinding::new("secondary-shift-a", AttachFiles, Some("Workspace")),
        KeyBinding::new("secondary-b", ToggleSidebar, Some("Workspace")),
        KeyBinding::new("secondary-shift-d", ToggleDiff, Some("Workspace")),
        KeyBinding::new("secondary-alt-d", ToggleDiffScope, Some("Workspace")),
        KeyBinding::new("ctrl-`", ToggleTerminal, Some("Workspace")),
        KeyBinding::new("secondary-.", StopThread, Some("Workspace")),
        KeyBinding::new(
            "secondary-shift-backspace",
            ArchiveThread,
            Some("Workspace"),
        ),
        KeyBinding::new("secondary-/", ToggleShortcuts, Some("Workspace")),
        KeyBinding::new("secondary-shift-,", ReloadConfig, Some("Workspace")),
        KeyBinding::new("secondary-,", OpenConfigFolder, Some("Workspace")),
        KeyBinding::new("secondary-q", Quit, None),
    ];
    if cfg!(target_os = "macos") {
        bindings.push(KeyBinding::new("cmd-j", ToggleTerminal, Some("Workspace")));
        bindings.push(KeyBinding::new("cmd-k", ClearTerminal, Some("Terminal")));
    } else {
        bindings.push(KeyBinding::new(
            "ctrl-shift-k",
            ClearTerminal,
            Some("Terminal"),
        ));
    }
    // Keys the shell needs more than the app does: inside the terminal they go to the shell.
    for keys in terminal_keys() {
        bindings.push(KeyBinding::new(keys, NoAction, Some("Terminal")));
    }
    bindings
}

/// Workspace keys that the terminal passes to the shell instead. On macOS the app's keys
/// use cmd, so only a few clash; elsewhere they use ctrl, which shells use for editing.
fn terminal_keys() -> &'static [&'static str] {
    if cfg!(target_os = "macos") {
        &["alt-up", "alt-down", "f2"]
    } else {
        &[
            "f2", "ctrl-n", "ctrl-l", "ctrl-b", "ctrl-]", "ctrl-[", "ctrl-.", "ctrl-/", "ctrl-,",
            "ctrl-q", "alt-up", "alt-down",
        ]
    }
}

#[derive(Deserialize)]
struct KeymapSection {
    #[serde(default)]
    context: Option<String>,
    #[serde(default)]
    bindings: serde_json::Map<String, serde_json::Value>,
}

/// Replaces every binding with the defaults plus `keymap.json`. Returns a message when the
/// file had problems; good entries still apply.
pub fn load(cx: &mut App) -> Option<String> {
    cx.clear_key_bindings();
    cx.bind_keys(default_bindings());
    let path = config::config_file("keymap.json");
    let Ok(text) = std::fs::read_to_string(&path) else {
        return None;
    };
    match user_bindings(&text, cx) {
        Ok((bindings, errors)) => {
            cx.bind_keys(bindings);
            (!errors.is_empty()).then(|| format!("keymap.json: {}", errors.join("; ")))
        }
        Err(error) => Some(format!("keymap.json: {error:#}")),
    }
}

fn user_bindings(text: &str, cx: &App) -> Result<(Vec<KeyBinding>, Vec<String>)> {
    let sections: Vec<KeymapSection> =
        serde_json::from_str(text).context("expected a list of { context, bindings }")?;
    let mut bindings = Vec::new();
    let mut errors = Vec::new();
    for section in sections {
        let predicate = match section.context.as_deref().filter(|c| !c.is_empty()) {
            Some(context) => match KeyBindingContextPredicate::parse(context) {
                Ok(predicate) => Some(Rc::new(predicate)),
                Err(error) => {
                    errors.push(format!("context {context:?}: {error}"));
                    continue;
                }
            },
            None => None,
        };
        for (keys, target) in section.bindings {
            match binding(&keys, &target, predicate.clone(), cx) {
                Ok(binding) => bindings.push(binding),
                Err(error) => errors.push(format!("{keys:?}: {error:#}")),
            }
        }
    }
    Ok((bindings, errors))
}

/// Accepts `yat3::` action names from before the app was called YATT.
fn current_name(name: &str) -> String {
    match name.strip_prefix("yat3::") {
        Some(rest) => format!("yatt::{rest}"),
        None => name.to_string(),
    }
}

fn binding(
    keys: &str,
    target: &serde_json::Value,
    predicate: Option<Rc<KeyBindingContextPredicate>>,
    cx: &App,
) -> Result<KeyBinding> {
    let action: Box<dyn Action> = match target {
        serde_json::Value::Null => Box::new(NoAction),
        serde_json::Value::String(name) => cx
            .build_action(&current_name(name), None)
            .map_err(|error| anyhow!("{error}"))?,
        // ["action::Name", { ...arguments }], as in Zed.
        serde_json::Value::Array(parts) => {
            let name = parts
                .first()
                .and_then(|v| v.as_str())
                .ok_or_else(|| anyhow!("expected [\"action::Name\", {{..}}]"))?;
            cx.build_action(&current_name(name), parts.get(1).cloned())
                .map_err(|error| anyhow!("{error}"))?
        }
        _ => return Err(anyhow!("expected an action name or null")),
    };
    KeyBinding::load(keys, action, predicate, false, None, &DummyKeyboardMapper)
        .map_err(|error| anyhow!("{error}"))
}
