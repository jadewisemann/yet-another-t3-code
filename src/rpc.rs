//! A small client for the Effect RPC protocol that T3 Code serves on `/ws`.
//!
//! Wire format (`RpcSerialization.layerJson`, one JSON message per WebSocket frame):
//!
//! - client → server: `{"_tag":"Request","id":"1","tag":"<method>","payload":{..},"headers":[]}`,
//!   `{"_tag":"Ack","requestId":"1"}` after each stream chunk (the server waits for it),
//!   `{"_tag":"Interrupt","requestId":"1"}` to end a stream, and `{"_tag":"Ping"}`.
//! - server → client: `{"_tag":"Chunk","requestId":"1","values":[..]}` for streams,
//!   `{"_tag":"Exit","requestId":"1","exit":{"_tag":"Success","value":..}}` or a `Failure`
//!   with a `cause` array, `{"_tag":"Defect",..}`, and `{"_tag":"Pong"}` heartbeats.
//!
//! The socket runs on its own thread with a small tokio runtime. The UI talks to it through
//! channels, so nothing here touches GPUI.

use std::collections::HashMap;
use std::time::Duration;

use anyhow::{Result, anyhow};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::{self, Message, client::IntoClientRequest};

use crate::config::Connection;
use crate::protocol::{self, ShellStreamItem, ThreadStreamItem, method};

#[derive(Debug)]
#[allow(clippy::large_enum_variant)]
pub enum RpcEvent {
    Connecting,
    Connected,
    Disconnected(String),
    /// The server no longer accepts the saved token; the user has to pair again.
    Unauthorized(String),
    Shell(ShellStreamItem),
    Thread {
        thread_id: String,
        item: ThreadStreamItem,
    },
    /// A command the user started failed. `label` says which, for the status line.
    CommandFailed {
        label: String,
        error: String,
    },
    /// A launched thread exists; the UI can select it.
    Launched {
        thread_id: Option<String>,
    },
}

#[derive(Debug)]
enum RpcCommand {
    SubscribeThread(Option<String>),
    Dispatch { label: String, command: Value },
    Launch { input: Value },
}

#[derive(Clone)]
pub struct RpcHandle {
    tx: mpsc::UnboundedSender<RpcCommand>,
}

impl RpcHandle {
    /// Follows one thread at a time; `None` stops following.
    pub fn subscribe_thread(&self, thread_id: Option<String>) {
        let _ = self.tx.send(RpcCommand::SubscribeThread(thread_id));
    }

    pub fn dispatch(&self, label: impl Into<String>, command: Value) {
        let _ = self.tx.send(RpcCommand::Dispatch {
            label: label.into(),
            command,
        });
    }

    pub fn launch_thread(&self, input: Value) {
        let _ = self.tx.send(RpcCommand::Launch { input });
    }
}

pub fn start(connection: Connection, events: async_channel::Sender<RpcEvent>) -> RpcHandle {
    let (tx, rx) = mpsc::unbounded_channel();
    std::thread::Builder::new()
        .name("yatt-rpc".into())
        .spawn(move || {
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("tokio runtime");
            runtime.block_on(run(connection, rx, events));
        })
        .expect("spawn rpc thread");
    RpcHandle { tx }
}

enum Pending {
    Shell,
    Thread(String),
    Call(String),
    Launch,
}

enum SessionEnd {
    /// The UI went away.
    Closed,
    Unauthorized(String),
    Lost(String),
}

async fn run(
    connection: Connection,
    mut commands: mpsc::UnboundedReceiver<RpcCommand>,
    events: async_channel::Sender<RpcEvent>,
) {
    let mut followed_thread: Option<String> = None;
    let mut backoff = Duration::from_millis(500);
    loop {
        let _ = events.send(RpcEvent::Connecting).await;
        match session(&connection, &mut commands, &events, &mut followed_thread).await {
            SessionEnd::Closed => return,
            SessionEnd::Unauthorized(reason) => {
                let _ = events.send(RpcEvent::Unauthorized(reason)).await;
                return;
            }
            SessionEnd::Lost(reason) => {
                let _ = events.send(RpcEvent::Disconnected(reason)).await;
            }
        }
        // Keep the latest thread choice while waiting, and stop if the UI is gone.
        let sleep = tokio::time::sleep(backoff);
        tokio::pin!(sleep);
        loop {
            tokio::select! {
                _ = &mut sleep => break,
                command = commands.recv() => match command {
                    None => return,
                    Some(RpcCommand::SubscribeThread(id)) => followed_thread = id,
                    Some(RpcCommand::Dispatch { label, .. }) => {
                        let _ = events.send(RpcEvent::CommandFailed {
                            label,
                            error: "not connected".into(),
                        }).await;
                    }
                    Some(RpcCommand::Launch { .. }) => {
                        let _ = events.send(RpcEvent::CommandFailed {
                            label: "New thread".into(),
                            error: "not connected".into(),
                        }).await;
                    }
                },
            }
        }
        backoff = (backoff * 2).min(Duration::from_secs(15));
    }
}

async fn session(
    connection: &Connection,
    commands: &mut mpsc::UnboundedReceiver<RpcCommand>,
    events: &async_channel::Sender<RpcEvent>,
    followed_thread: &mut Option<String>,
) -> SessionEnd {
    let request = match build_request(connection) {
        Ok(request) => request,
        Err(error) => return SessionEnd::Lost(error.to_string()),
    };
    let socket = match tokio_tungstenite::connect_async(request).await {
        Ok((socket, _)) => socket,
        Err(tungstenite::Error::Http(response)) => {
            let status = response.status().as_u16();
            let body = response
                .body()
                .as_ref()
                .map(|bytes| String::from_utf8_lossy(bytes).into_owned())
                .unwrap_or_default();
            return match status {
                401 | 403 => SessionEnd::Unauthorized(format!("HTTP {status} {body}")),
                426 => SessionEnd::Unauthorized(format!(
                    "this server speaks another orchestration protocol: {body}"
                )),
                _ => SessionEnd::Lost(format!("HTTP {status} {body}")),
            };
        }
        Err(error) => return SessionEnd::Lost(error.to_string()),
    };
    let _ = events.send(RpcEvent::Connected).await;
    let (mut sink, mut stream) = socket.split();

    let mut next_id: u64 = 1;
    let mut pending: HashMap<String, Pending> = HashMap::new();
    let mut thread_request: Option<String> = None;

    macro_rules! send {
        ($value:expr) => {
            if let Err(error) = sink.send(Message::text($value.to_string())).await {
                return SessionEnd::Lost(error.to_string());
            }
        };
    }
    macro_rules! request {
        ($tag:expr, $payload:expr, $pending:expr) => {{
            let payload: Value = $payload;
            let id = next_id.to_string();
            next_id += 1;
            pending.insert(id.clone(), $pending);
            send!(json!({
                "_tag": "Request",
                "id": id,
                "tag": $tag,
                "payload": payload,
                "headers": [],
            }));
            id
        }};
    }

    request!(method::SUBSCRIBE_SHELL, json!({}), Pending::Shell);
    if let Some(thread_id) = followed_thread.clone() {
        thread_request = Some(request!(
            method::SUBSCRIBE_THREAD,
            json!({ "threadId": thread_id }),
            Pending::Thread(thread_id)
        ));
    }

    let mut ping = tokio::time::interval(Duration::from_secs(20));
    ping.tick().await;

    loop {
        tokio::select! {
            _ = ping.tick() => {
                send!(json!({ "_tag": "Ping" }));
            }
            command = commands.recv() => {
                let Some(command) = command else { return SessionEnd::Closed };
                match command {
                    RpcCommand::SubscribeThread(thread_id) => {
                        if *followed_thread == thread_id && thread_request.is_some() {
                            continue;
                        }
                        if let Some(old) = thread_request.take() {
                            pending.remove(&old);
                            send!(json!({ "_tag": "Interrupt", "requestId": old }));
                        }
                        *followed_thread = thread_id.clone();
                        if let Some(thread_id) = thread_id {
                            thread_request = Some(request!(
                                method::SUBSCRIBE_THREAD,
                                json!({ "threadId": thread_id }),
                                Pending::Thread(thread_id)
                            ));
                        }
                    }
                    RpcCommand::Dispatch { label, command } => {
                        request!(method::DISPATCH_COMMAND, command, Pending::Call(label));
                    }
                    RpcCommand::Launch { input } => {
                        request!(method::LAUNCH_THREAD, input, Pending::Launch);
                    }
                }
            }
            frame = stream.next() => {
                let text = match frame {
                    None => return SessionEnd::Lost("connection closed".into()),
                    Some(Err(error)) => return SessionEnd::Lost(error.to_string()),
                    Some(Ok(Message::Text(text))) => text.to_string(),
                    Some(Ok(Message::Binary(bytes))) => String::from_utf8_lossy(&bytes).into_owned(),
                    Some(Ok(Message::Close(frame))) => {
                        return SessionEnd::Lost(
                            frame.map(|f| f.reason.to_string()).unwrap_or_else(|| "closed".into()),
                        );
                    }
                    Some(Ok(_)) => continue,
                };
                let Ok(decoded) = serde_json::from_str::<Value>(&text) else { continue };
                let messages = match decoded {
                    Value::Array(items) => items,
                    other => vec![other],
                };
                for message in messages {
                    let tag = message.get("_tag").and_then(Value::as_str).unwrap_or("");
                    let request_id = message.get("requestId").map(request_id_string);
                    match tag {
                        "Chunk" => {
                            let Some(request_id) = request_id else { continue };
                            let values = message.get("values").and_then(Value::as_array).cloned().unwrap_or_default();
                            match pending.get(&request_id) {
                                Some(Pending::Shell) => {
                                    for value in values {
                                        if let Ok(item) = serde_json::from_value::<ShellStreamItem>(value) {
                                            let _ = events.send(RpcEvent::Shell(item)).await;
                                        }
                                    }
                                }
                                Some(Pending::Thread(thread_id)) => {
                                    for value in values {
                                        if let Ok(item) = serde_json::from_value::<ThreadStreamItem>(value) {
                                            let _ = events.send(RpcEvent::Thread {
                                                thread_id: thread_id.clone(),
                                                item,
                                            }).await;
                                        }
                                    }
                                }
                                _ => {}
                            }
                            // Streams stall until the client acknowledges each chunk.
                            send!(json!({ "_tag": "Ack", "requestId": request_id }));
                        }
                        "Exit" => {
                            let Some(request_id) = request_id else { continue };
                            let Some(entry) = pending.remove(&request_id) else { continue };
                            let exit = message.get("exit").cloned().unwrap_or(Value::Null);
                            let failure = exit_failure(&exit);
                            match entry {
                                Pending::Shell => {
                                    return SessionEnd::Lost(
                                        failure.unwrap_or_else(|| "shell stream ended".into()),
                                    );
                                }
                                Pending::Thread(thread_id) => {
                                    if thread_request.as_deref() == Some(request_id.as_str()) {
                                        thread_request = None;
                                    }
                                    if let Some(error) = failure {
                                        let _ = events.send(RpcEvent::CommandFailed {
                                            label: format!("Open thread {thread_id}"),
                                            error,
                                        }).await;
                                    }
                                }
                                Pending::Call(label) => {
                                    if let Some(error) = failure {
                                        let _ = events.send(RpcEvent::CommandFailed { label, error }).await;
                                    }
                                }
                                Pending::Launch => match failure {
                                    Some(error) => {
                                        let _ = events.send(RpcEvent::CommandFailed {
                                            label: "New thread".into(),
                                            error,
                                        }).await;
                                    }
                                    None => {
                                        let thread_id = exit
                                            .pointer("/value/threadId")
                                            .and_then(Value::as_str)
                                            .map(str::to_owned);
                                        let _ = events.send(RpcEvent::Launched { thread_id }).await;
                                    }
                                },
                            }
                        }
                        "Defect" => {
                            let defect = message.get("defect").map(Value::to_string).unwrap_or_default();
                            return SessionEnd::Lost(format!("server defect: {defect}"));
                        }
                        "ClientProtocolError" => {
                            let error = message.get("error").map(Value::to_string).unwrap_or_default();
                            return SessionEnd::Lost(format!("protocol error: {error}"));
                        }
                        _ => {}
                    }
                }
            }
        }
    }
}

pub fn build_request(connection: &Connection) -> Result<tungstenite::handshake::client::Request> {
    let mut request = connection.websocket_url()?.into_client_request()?;
    let header = format!("Bearer {}", connection.token)
        .parse()
        .map_err(|_| anyhow!("the saved token has characters a header cannot carry"))?;
    request.headers_mut().insert("authorization", header);
    Ok(request)
}

fn request_id_string(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => other.to_string(),
    }
}

/// Describes a failed `Exit`, or `None` when it succeeded.
fn exit_failure(exit: &Value) -> Option<String> {
    if exit.get("_tag").and_then(Value::as_str) != Some("Failure") {
        return None;
    }
    let causes = exit.get("cause").and_then(Value::as_array);
    let text = causes
        .into_iter()
        .flatten()
        .map(|cause| {
            let error = cause.get("error").or_else(|| cause.get("defect"));
            match error {
                Some(error) => error
                    .get("message")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
                    .or_else(|| error.get("_tag").and_then(Value::as_str).map(str::to_owned))
                    .unwrap_or_else(|| error.to_string()),
                None => cause
                    .get("_tag")
                    .and_then(Value::as_str)
                    .unwrap_or("failed")
                    .to_string(),
            }
        })
        .collect::<Vec<_>>()
        .join("; ");
    Some(if text.is_empty() {
        "failed".into()
    } else {
        text
    })
}

#[allow(dead_code)]
pub fn launch_defaults_from(
    thread: Option<&protocol::ThreadShell>,
    project: Option<&protocol::ProjectShell>,
) -> Option<protocol::LaunchDefaults> {
    let model_selection = thread
        .and_then(|t| t.model_selection.clone())
        .or_else(|| project.and_then(|p| p.default_model_selection.clone()))
        .filter(|v| !v.is_null())?;
    Some(protocol::LaunchDefaults {
        model_selection,
        runtime_mode: thread
            .and_then(|t| t.runtime_mode.clone())
            .unwrap_or_else(|| "full-access".into()),
        interaction_mode: thread
            .and_then(|t| t.interaction_mode.clone())
            .unwrap_or_else(|| "default".into()),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_failures() {
        assert_eq!(
            exit_failure(&json!({ "_tag": "Success", "value": null })),
            None
        );
        let failed = json!({
            "_tag": "Failure",
            "cause": [{ "_tag": "Fail", "error": { "_tag": "OrchestrationV2DispatchCommandError", "message": "nope" } }]
        });
        assert_eq!(exit_failure(&failed).as_deref(), Some("nope"));
    }
}

/// Opens a socket with the saved token and closes it again. Used by `yatt pair` to prove the
/// token works.
pub fn check(connection: &Connection) -> Result<()> {
    let request = build_request(connection)?;
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()?;
    runtime.block_on(async move {
        match tokio::time::timeout(
            std::time::Duration::from_secs(15),
            tokio_tungstenite::connect_async(request),
        )
        .await
        {
            Ok(Ok((mut socket, _))) => {
                let _ = socket.close(None).await;
                Ok(())
            }
            Ok(Err(tungstenite::Error::Http(response))) => {
                Err(anyhow!("the server answered HTTP {}", response.status()))
            }
            Ok(Err(error)) => Err(anyhow!(error)),
            Err(_) => Err(anyhow!("the server did not answer within 15 seconds")),
        }
    })
}
