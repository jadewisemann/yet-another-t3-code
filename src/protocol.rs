//! The slice of T3 Code's orchestration protocol (v2) this client uses.
//!
//! Source of truth: `packages/contracts/src/orchestrationV2.ts` and
//! `packages/contracts/src/rpc.ts` in pingdotgg/t3code. Types here decode leniently: unknown
//! fields are ignored and most fields default, so newer servers keep working until they
//! change something this client reads.

use serde::Deserialize;
use serde_json::{Value, json};

pub const ORCHESTRATION_PROTOCOL_VERSION: &str = "2";

pub mod method {
    pub const DISPATCH_COMMAND: &str = "orchestration.dispatchCommand";
    pub const LAUNCH_THREAD: &str = "orchestration.launchThread";
    pub const SUBSCRIBE_SHELL: &str = "orchestration.subscribeShell";
    pub const SUBSCRIBE_THREAD: &str = "orchestration.subscribeThread";
    pub const PROJECTS_MUTATE: &str = "projects.mutate";
    pub const GET_TURN_DIFF: &str = "orchestration.getTurnDiff";
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ProjectShell {
    pub id: String,
    pub title: String,
    #[serde(default)]
    pub default_model_selection: Option<Value>,
    #[serde(default)]
    pub archived_at: Option<String>,
    #[serde(default)]
    pub pinned_at: Option<String>,
}

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Lineage {
    #[serde(default)]
    pub relationship_to_parent: Option<String>,
    #[serde(default)]
    pub side_chat: Option<bool>,
    #[serde(default)]
    pub workflow_step: Option<bool>,
}

impl Lineage {
    /// Mirrors `isListedThreadLineage`: subagents, side chats and workflow steps live
    /// inside their parent, never in thread lists.
    pub fn is_listed(&self) -> bool {
        self.relationship_to_parent.as_deref() != Some("subagent")
            && self.side_chat != Some(true)
            && self.workflow_step != Some(true)
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadShell {
    pub id: String,
    pub project_id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub model_selection: Option<Value>,
    #[serde(default)]
    pub runtime_mode: Option<String>,
    #[serde(default)]
    pub interaction_mode: Option<String>,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub lineage: Lineage,
    #[serde(default)]
    pub archived_at: Option<String>,
    #[serde(default)]
    pub deleted_at: Option<String>,
    #[serde(default)]
    pub pinned_at: Option<String>,
    #[serde(default)]
    pub updated_at: String,
    #[serde(default)]
    pub latest_user_message_at: Option<String>,
    #[serde(default)]
    pub pending_runtime_request: Option<Value>,
    #[serde(default)]
    pub branch: Option<String>,
    #[serde(default)]
    pub worktree_path: Option<String>,
}

impl ThreadShell {
    pub fn is_listed(&self) -> bool {
        self.lineage.is_listed() && self.archived_at.is_none() && self.deleted_at.is_none()
    }

    pub fn is_working(&self) -> bool {
        matches!(
            self.status.as_str(),
            "queued" | "preparing" | "starting" | "running" | "waiting"
        )
    }

    pub fn needs_attention(&self) -> bool {
        self.pending_runtime_request
            .as_ref()
            .is_some_and(|v| !v.is_null())
    }

    /// The time the sidebar sorts by: the last thing the user did, else the last change.
    pub fn sort_key(&self) -> &str {
        self.latest_user_message_at
            .as_deref()
            .unwrap_or(self.updated_at.as_str())
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ShellSnapshot {
    #[serde(default)]
    pub projects: Vec<ProjectShell>,
    #[serde(default)]
    pub threads: Vec<ThreadShell>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(tag = "kind")]
#[allow(clippy::large_enum_variant)]
pub enum ShellStreamItem {
    #[serde(rename = "synchronized")]
    Synchronized,
    #[serde(rename = "snapshot")]
    Snapshot { snapshot: ShellSnapshot },
    #[serde(rename = "project.updated")]
    ProjectUpdated { project: ProjectShell },
    #[serde(rename = "project.removed")]
    ProjectRemoved {
        #[serde(rename = "projectId")]
        project_id: String,
    },
    #[serde(rename = "thread.updated")]
    ThreadUpdated {
        location: String,
        thread: ThreadShell,
    },
    #[serde(rename = "thread.removed")]
    ThreadRemoved {
        #[serde(rename = "threadId")]
        thread_id: String,
    },
    #[serde(other)]
    Unknown,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ConversationMessage {
    pub id: String,
    pub role: String,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub streaming: bool,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub workflow_script: Option<bool>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadProjection {
    #[serde(default)]
    pub messages: Vec<ConversationMessage>,
    #[serde(default)]
    pub turn_items: Vec<Value>,
    #[serde(default)]
    pub runs: Vec<Value>,
    #[serde(default)]
    pub checkpoints: Vec<Value>,
}

/// What the diff view needs to know about a thread: which runs completed and which turns
/// have a checkpoint. Turn numbers are the checkpoints' `appRunOrdinal`.
#[derive(Clone, Debug, Default)]
pub struct TurnLedger {
    completed_runs: std::collections::HashSet<String>,
    /// Ready checkpoints as (run id, turn).
    checkpoints: Vec<(String, u64)>,
}

impl TurnLedger {
    pub fn from_projection(projection: &ThreadProjection) -> Self {
        let mut ledger = Self::default();
        for run in &projection.runs {
            ledger.update_run(run);
        }
        for checkpoint in &projection.checkpoints {
            ledger.update_checkpoint(checkpoint);
        }
        ledger
    }

    /// Folds a thread event in; true when the latest turn may have changed.
    pub fn apply_event(&mut self, event: &Value) -> bool {
        let Some(payload) = event.get("payload") else {
            return false;
        };
        match event.get("type").and_then(Value::as_str) {
            Some("run.created" | "run.updated") => self.update_run(payload),
            Some("checkpoint.captured") => self.update_checkpoint(payload),
            _ => return false,
        }
        true
    }

    fn update_run(&mut self, run: &Value) {
        let Some(id) = run.get("id").and_then(Value::as_str) else {
            return;
        };
        if run.get("status").and_then(Value::as_str) == Some("completed") {
            self.completed_runs.insert(id.to_string());
        } else {
            self.completed_runs.remove(id);
        }
    }

    fn update_checkpoint(&mut self, checkpoint: &Value) {
        let ready = checkpoint.get("status").and_then(Value::as_str) == Some("ready");
        let run = checkpoint.get("runId").and_then(Value::as_str);
        let turn = checkpoint.get("appRunOrdinal").and_then(Value::as_u64);
        if let (true, Some(run), Some(turn)) = (ready, run, turn)
            && !self.checkpoints.contains(&(run.to_string(), turn))
        {
            self.checkpoints.push((run.to_string(), turn));
        }
    }

    /// The last turn whose changes can be shown, or 0 before any.
    pub fn latest_turn(&self) -> u64 {
        self.checkpoints
            .iter()
            .filter(|(run, _)| self.completed_runs.contains(run))
            .map(|(_, turn)| *turn)
            .max()
            .unwrap_or(0)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DiffLineKind {
    /// `diff --git a/x b/x`: starts a file.
    File,
    /// `index`, `new file mode`, `---`, `+++` and the like.
    Meta,
    Hunk,
    Added,
    Removed,
    Context,
}

/// Splits a unified diff into lines and says what each one is.
pub fn diff_lines(diff: &str) -> Vec<(DiffLineKind, String)> {
    let mut in_header = false;
    diff.lines()
        .map(|line| {
            let kind = if line.starts_with("diff --git ") {
                in_header = true;
                DiffLineKind::File
            } else if line.starts_with("@@") {
                in_header = false;
                DiffLineKind::Hunk
            } else if in_header {
                DiffLineKind::Meta
            } else if line.starts_with('+') {
                DiffLineKind::Added
            } else if line.starts_with('-') {
                DiffLineKind::Removed
            } else {
                DiffLineKind::Context
            };
            (kind, line.to_string())
        })
        .collect()
}

/// The path a `diff --git a/<path> b/<path>` line names.
pub fn diff_file_path(line: &str) -> &str {
    line.rsplit_once(" b/")
        .map(|(_, path)| path)
        .unwrap_or(line)
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct QuestionOption {
    #[serde(default)]
    pub label: String,
    #[serde(default)]
    pub description: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Question {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub header: String,
    #[serde(default)]
    pub question: String,
    #[serde(default)]
    pub options: Vec<QuestionOption>,
}

/// A turn item that waits for the user: an approval (`approval_request`) or questions
/// (`user_input_request`).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RequestItem {
    pub id: String,
    #[serde(rename = "type")]
    pub item_type: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub request_id: Option<String>,
    #[serde(default)]
    pub request_kind: Option<String>,
    #[serde(default)]
    pub prompt: Option<String>,
    #[serde(default)]
    pub questions: Vec<Question>,
}

impl RequestItem {
    pub fn is_waiting(&self) -> bool {
        self.status == "waiting" && self.request_id.is_some()
    }

    pub fn is_question(&self) -> bool {
        self.item_type == "user_input_request"
    }
}

/// Reads a turn item if it is a request for the user.
pub fn request_from_item(item: &Value) -> Option<RequestItem> {
    let kind = item.get("type")?.as_str()?;
    if kind != "approval_request" && kind != "user_input_request" {
        return None;
    }
    serde_json::from_value(item.clone()).ok()
}

/// Reads a `turn-item.updated` event if its item is a request for the user.
pub fn request_from_event(event: &Value) -> Option<RequestItem> {
    if event.get("type")?.as_str()? != "turn-item.updated" {
        return None;
    }
    request_from_item(event.get("payload")?)
}

#[derive(Clone, Debug, Deserialize)]
#[serde(tag = "kind")]
pub enum ThreadStreamItem {
    #[serde(rename = "synchronized")]
    Synchronized,
    #[serde(rename = "snapshot")]
    Snapshot { projection: ThreadProjection },
    #[serde(rename = "event")]
    Event { event: Value },
    #[serde(other)]
    Unknown,
}

/// The only thread events this client reads; everything else is ignored.
pub fn message_from_event(event: &Value) -> Option<ConversationMessage> {
    if event.get("type")?.as_str()? != "message.updated" {
        return None;
    }
    serde_json::from_value(event.get("payload")?.clone()).ok()
}

pub fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

/// The current time as `2026-11-24T08:00:00.000Z`, the form the server's IsoDateTime takes.
pub fn iso_now() -> String {
    let millis = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0);
    iso_from_unix_millis(millis)
}

fn iso_from_unix_millis(millis: i64) -> String {
    let secs = millis.div_euclid(1000);
    let ms = millis.rem_euclid(1000);
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    // Howard Hinnant's civil_from_days.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{ms:03}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60
    )
}

pub fn visit_thread(thread_id: &str) -> Value {
    json!({
        "type": "thread.visit",
        "commandId": new_id(),
        "threadId": thread_id,
        "visitedAt": iso_now(),
        "opened": true,
    })
}

pub fn rename_thread(thread_id: &str, title: &str) -> Value {
    json!({
        "type": "thread.metadata.update",
        "commandId": new_id(),
        "threadId": thread_id,
        "title": title,
    })
}

pub fn send_message(thread_id: &str, text: &str, thread_is_working: bool) -> Value {
    let command_id = new_id();
    // Same choice as the web composer: a message sent while a run is going waits for it.
    let dispatch_mode = if thread_is_working {
        json!({ "type": "queue_after_active" })
    } else {
        json!({ "type": "start_immediately" })
    };
    json!({
        "type": "message.dispatch",
        "commandId": command_id,
        "createdBy": "user",
        "creationSource": "web",
        "threadId": thread_id,
        "messageId": command_id,
        "text": text,
        "attachments": [],
        "dispatchMode": dispatch_mode,
    })
}

/// Answers an approval: `accept`, `acceptForSession` or `decline`.
pub fn respond_approval(thread_id: &str, request_id: &str, decision: &str) -> Value {
    json!({
        "type": "runtime-request.respond",
        "commandId": new_id(),
        "threadId": thread_id,
        "requestId": request_id,
        "decision": decision,
    })
}

/// Answers questions, keyed by question id.
pub fn answer_questions(
    thread_id: &str,
    request_id: &str,
    answers: &std::collections::BTreeMap<String, String>,
) -> Value {
    json!({
        "type": "runtime-request.respond",
        "commandId": new_id(),
        "threadId": thread_id,
        "requestId": request_id,
        "answers": answers,
    })
}

pub fn stop_thread(thread_id: &str) -> Value {
    json!({
        "type": "thread.stop",
        "commandId": new_id(),
        "threadId": thread_id,
    })
}

pub fn archive_thread(thread_id: &str) -> Value {
    json!({
        "type": "thread.archive",
        "commandId": new_id(),
        "threadId": thread_id,
    })
}

/// Adds a folder as a project. The title is the folder's name.
pub fn create_project(project_id: &str, workspace_root: &std::path::Path) -> Value {
    let title = workspace_root
        .file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .filter(|name| !name.is_empty())
        .unwrap_or_else(|| workspace_root.display().to_string());
    json!({
        "type": "project.create",
        "commandId": new_id(),
        "projectId": project_id,
        "title": title,
        "workspaceRoot": workspace_root.display().to_string(),
    })
}

/// Codex's default model, for a first thread when no thread or project names one.
pub const DEFAULT_MODEL_SELECTION: (&str, &str) = ("codex", "gpt-6-astra");

/// The models a new thread can start on, as (provider instance, model, label). These are the
/// current models upstream's model manifest lists for the Codex and Claude providers.
pub const MODEL_CHOICES: &[(&str, &str, &str)] = &[
    ("codex", "gpt-6-astra", "GPT-6 Astra"),
    ("codex", "gpt-6.1-sol", "GPT-6.1 Sol"),
    ("codex", "gpt-6-luna", "GPT-6 Luna"),
    ("claudeAgent", "claude-opus-5-5", "Claude Opus 5.5"),
    ("claudeAgent", "claude-sonnet-5-5", "Claude Sonnet 5.5"),
    ("claudeAgent", "claude-fable-5-2", "Claude Fable 5.2"),
    ("claudeAgent", "claude-haiku-5-5", "Claude Haiku 5.5"),
];

pub fn model_selection(instance: &str, model: &str) -> Value {
    json!({ "instanceId": instance, "model": model })
}

/// The label of a model selection: its name in MODEL_CHOICES, else the model id.
pub fn model_label(selection: &Value) -> String {
    let instance = selection
        .get("instanceId")
        .or_else(|| selection.get("provider"))
        .and_then(Value::as_str)
        .unwrap_or("");
    let model = selection.get("model").and_then(Value::as_str).unwrap_or("");
    MODEL_CHOICES
        .iter()
        .find(|(i, m, _)| *i == instance && *m == model)
        .map(|(_, _, label)| label.to_string())
        .unwrap_or_else(|| {
            if model.is_empty() {
                "Default model".into()
            } else {
                model.to_string()
            }
        })
}

/// The model after `selection` in MODEL_CHOICES, wrapping around; the first one when
/// `selection` is not listed.
pub fn next_model(selection: &Value) -> Value {
    let instance = selection
        .get("instanceId")
        .and_then(Value::as_str)
        .unwrap_or("");
    let model = selection.get("model").and_then(Value::as_str).unwrap_or("");
    let next = MODEL_CHOICES
        .iter()
        .position(|(i, m, _)| *i == instance && *m == model)
        .map_or(0, |i| (i + 1) % MODEL_CHOICES.len());
    let (i, m, _) = MODEL_CHOICES[next];
    model_selection(i, m)
}

pub struct LaunchDefaults {
    pub model_selection: Value,
    pub runtime_mode: String,
    pub interaction_mode: String,
    /// Start the thread in a new git worktree on a temporary branch, from HEAD.
    pub new_worktree: bool,
}

pub fn launch_thread(project_id: &str, defaults: &LaunchDefaults, first_message: &str) -> Value {
    let title: String = first_message.chars().take(60).collect();
    let title = if title.trim().is_empty() {
        "New thread".to_string()
    } else {
        title.trim().to_string()
    };
    json!({
        "commandId": new_id(),
        "creationSource": "web",
        "projectId": project_id,
        "title": title,
        "generateTitle": true,
        "modelSelection": defaults.model_selection,
        "runtimeMode": defaults.runtime_mode,
        "interactionMode": defaults.interaction_mode,
        "workspaceStrategy": if defaults.new_worktree {
            json!({ "type": "worktree", "baseRef": "HEAD" })
        } else {
            json!({ "type": "root" })
        },
        "initialMessage": {
            "messageId": new_id(),
            "text": first_message,
            "attachments": [],
        },
    })
}

#[cfg(test)]
mod model_tests {
    use super::*;

    #[test]
    fn cycles_models_and_labels_them() {
        let first = model_selection("codex", "gpt-6-astra");
        assert_eq!(model_label(&first), "GPT-6 Astra");
        let last = model_selection("claudeAgent", "claude-haiku-5-5");
        assert_eq!(next_model(&last), first);
        assert_eq!(next_model(&model_selection("codex", "unknown")), first);
        assert_eq!(model_label(&model_selection("codex", "gpt-9")), "gpt-9");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn launches_in_a_worktree() {
        let mut defaults = LaunchDefaults {
            model_selection: json!({"instanceId": "codex", "model": "m"}),
            runtime_mode: "full-access".into(),
            interaction_mode: "default".into(),
            new_worktree: false,
        };
        assert_eq!(
            launch_thread("p", &defaults, "hi")["workspaceStrategy"],
            json!({"type": "root"})
        );
        defaults.new_worktree = true;
        assert_eq!(
            launch_thread("p", &defaults, "hi")["workspaceStrategy"],
            json!({"type": "worktree", "baseRef": "HEAD"})
        );
    }

    #[test]
    fn reads_diff_lines() {
        let diff = "diff --git a/hello.txt b/hello.txt\nnew file mode 100644\n--- /dev/null\n+++ b/hello.txt\n@@ -0,0 +1,2 @@\n+hello\n-bye\n same\n";
        let kinds: Vec<_> = diff_lines(diff).into_iter().map(|(k, _)| k).collect();
        use DiffLineKind::*;
        assert_eq!(
            kinds,
            [File, Meta, Meta, Meta, Hunk, Added, Removed, Context]
        );
        assert_eq!(
            diff_file_path("diff --git a/src/a b.rs b/src/a b.rs"),
            "src/a b.rs"
        );
    }

    #[test]
    fn tracks_the_latest_turn() {
        let projection: ThreadProjection = serde_json::from_value(json!({
            "runs": [{"id": "r1", "status": "completed"}, {"id": "r2", "status": "running"}],
            "checkpoints": [
                {"runId": null, "appRunOrdinal": null, "status": "ready"},
                {"runId": "r1", "appRunOrdinal": 1, "status": "ready"},
            ],
        }))
        .unwrap();
        let mut ledger = TurnLedger::from_projection(&projection);
        assert_eq!(ledger.latest_turn(), 1);
        ledger.apply_event(&json!({"type": "checkpoint.captured", "payload": {"runId": "r2", "appRunOrdinal": 2, "status": "ready"}}));
        assert_eq!(ledger.latest_turn(), 1);
        ledger.apply_event(
            &json!({"type": "run.updated", "payload": {"id": "r2", "status": "completed"}}),
        );
        assert_eq!(ledger.latest_turn(), 2);
    }

    #[test]
    fn decodes_shell_items() {
        let item: ShellStreamItem = serde_json::from_value(json!({
            "kind": "thread.updated",
            "sequence": 4,
            "location": "active",
            "thread": {
                "id": "t1", "projectId": "p1", "title": "Hello", "status": "running",
                "lineage": { "parentThreadId": null, "relationshipToParent": null, "rootThreadId": "t1" },
                "archivedAt": null, "deletedAt": null, "updatedAt": "2026-11-24T00:00:00.000Z",
                "somethingNew": { "nested": true }
            }
        }))
        .unwrap();
        match item {
            ShellStreamItem::ThreadUpdated { thread, .. } => {
                assert!(thread.is_listed());
                assert!(thread.is_working());
            }
            other => panic!("unexpected {other:?}"),
        }
        let unknown: ShellStreamItem =
            serde_json::from_value(json!({ "kind": "future.thing", "x": 1 })).unwrap();
        assert!(matches!(unknown, ShellStreamItem::Unknown));
    }

    #[test]
    fn reads_message_events() {
        let event = json!({
            "type": "message.updated",
            "payload": { "id": "m1", "role": "assistant", "text": "hi", "streaming": true,
                         "createdAt": "2026-11-24T00:00:00.000Z" }
        });
        let message = message_from_event(&event).unwrap();
        assert_eq!(message.text, "hi");
        assert!(message_from_event(&json!({ "type": "run.updated", "payload": {} })).is_none());
    }

    #[test]
    fn formats_iso_times() {
        assert_eq!(iso_from_unix_millis(0), "1970-01-01T00:00:00.000Z");
        assert_eq!(
            iso_from_unix_millis(1_795_478_400_123),
            "2026-11-24T00:00:00.123Z"
        );
        assert_eq!(
            iso_from_unix_millis(951_782_400_000),
            "2000-02-29T00:00:00.000Z"
        );
    }

    #[test]
    fn subagents_are_not_listed() {
        let lineage = Lineage {
            relationship_to_parent: Some("subagent".into()),
            ..Default::default()
        };
        assert!(!lineage.is_listed());
    }

    #[test]
    fn reads_requests_from_turn_items() {
        let approval = json!({
            "type": "turn-item.updated",
            "payload": {
                "id": "turn-item:a", "type": "approval_request", "status": "waiting",
                "requestId": "r1", "requestKind": "command", "prompt": "run ls?"
            }
        });
        let item = request_from_event(&approval).expect("approval");
        assert!(item.is_waiting() && !item.is_question());
        assert_eq!(item.prompt.as_deref(), Some("run ls?"));
        let question = json!({
            "id": "turn-item:q", "type": "user_input_request", "status": "waiting", "requestId": "r2",
            "questions": [{ "id": "color", "header": "Color", "question": "Which color?",
                "options": [{ "label": "Red", "description": "" }] }]
        });
        let item = request_from_item(&question).expect("question");
        assert!(item.is_question());
        assert_eq!(item.questions[0].options[0].label, "Red");
        assert!(request_from_item(&json!({ "type": "user_message", "id": "x" })).is_none());
    }
}
