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
pub struct ThreadProjection {
    #[serde(default)]
    pub messages: Vec<ConversationMessage>,
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

pub struct LaunchDefaults {
    pub model_selection: Value,
    pub runtime_mode: String,
    pub interaction_mode: String,
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
        "workspaceStrategy": { "type": "root" },
        "initialMessage": {
            "messageId": new_id(),
            "text": first_message,
            "attachments": [],
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;

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
}
