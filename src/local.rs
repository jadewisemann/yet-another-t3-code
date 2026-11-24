//! Pairing with a T3 Code server on this machine without pasting a link.
//!
//! A running server records where it listens in `~/.t3/userdata/server-runtime.json`
//! (`$T3CODE_HOME` moves `~/.t3`). Minting a pairing token needs the server's own database,
//! so this runs the `t3 pair` command, the same one a person would type, and reads the token
//! from its output. The desktop app installs that command in `~/.local/bin` on first launch;
//! without it, `npx t3@<server version>` is used.

use std::io::Read as _;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{Context as _, Result, anyhow, bail};
use serde::Deserialize;

use crate::config::{self, Connection};

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeState {
    pub origin: String,
    #[serde(default)]
    pub server_version: Option<String>,
}

fn t3_home() -> Option<PathBuf> {
    match std::env::var_os("T3CODE_HOME") {
        Some(home) if !home.is_empty() => Some(PathBuf::from(home)),
        _ => dirs::home_dir().map(|home| home.join(".t3")),
    }
}

fn probe(origin: &str) -> bool {
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(2)))
        .build()
        .into();
    let url = format!(
        "{}/.well-known/t3/environment",
        origin.trim_end_matches('/')
    );
    agent.get(&url).call().is_ok()
}

/// The local server that is running and answering, if any.
pub fn running_server() -> Option<RuntimeState> {
    let home = t3_home()?;
    ["userdata", "dev"].into_iter().find_map(|variant| {
        let text = std::fs::read_to_string(home.join(variant).join("server-runtime.json")).ok()?;
        let state: RuntimeState = serde_json::from_str(&text).ok()?;
        probe(&state.origin).then_some(state)
    })
}

/// Opens the T3 Code desktop app and waits for its server. Returns false when the app is
/// not installed.
#[cfg(target_os = "macos")]
fn launch_desktop_app() -> bool {
    [
        "T3 Code (Beta)",
        "T3 Code",
        "T3 Code (Alpha)",
        "T3 Code (Nightly)",
    ]
    .into_iter()
    .any(|name| {
        Command::new("/usr/bin/open")
            .args(["-g", "-a", name])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .is_ok_and(|status| status.success())
    })
}

#[cfg(not(target_os = "macos"))]
fn launch_desktop_app() -> bool {
    false
}

fn wait_for_server(timeout: Duration) -> Option<RuntimeState> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if let Some(state) = running_server() {
            return Some(state);
        }
        std::thread::sleep(Duration::from_millis(1000));
    }
    None
}

/// Apps opened from Finder get a short PATH, so add the folders `t3`, node and npx usually
/// live in, then run through the login shell so its PATH setup applies too.
fn shell_command(script: &str) -> Command {
    let shell = std::env::var("SHELL")
        .ok()
        .filter(|shell| !shell.is_empty())
        .unwrap_or_else(|| "/bin/sh".into());
    let mut path: Vec<PathBuf> = Vec::new();
    if let Some(home) = dirs::home_dir() {
        for dir in [
            ".local/bin",
            "bin",
            ".bun/bin",
            ".volta/bin",
            ".npm-global/bin",
        ] {
            path.push(home.join(dir));
        }
    }
    path.push("/opt/homebrew/bin".into());
    path.push("/usr/local/bin".into());
    if let Some(existing) = std::env::var_os("PATH") {
        path.extend(std::env::split_paths(&existing));
    }
    let mut command = Command::new(shell);
    command.args(["-lc", script]);
    if let Ok(joined) = std::env::join_paths(path) {
        command.env("PATH", joined);
    }
    command
}

fn run(mut command: Command, timeout: Duration) -> Result<String> {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("could not start a shell")?;
    let mut stdout = child.stdout.take().expect("piped");
    let mut stderr = child.stderr.take().expect("piped");
    let out = std::thread::spawn(move || {
        let mut text = String::new();
        let _ = stdout.read_to_string(&mut text);
        text
    });
    let err = std::thread::spawn(move || {
        let mut text = String::new();
        let _ = stderr.read_to_string(&mut text);
        text
    });
    let deadline = Instant::now() + timeout;
    let status = loop {
        if let Some(status) = child.try_wait()? {
            break status;
        }
        if Instant::now() > deadline {
            let _ = child.kill();
            let _ = child.wait();
            bail!("`t3 pair` did not finish in {} seconds", timeout.as_secs());
        }
        std::thread::sleep(Duration::from_millis(100));
    };
    let stdout = out.join().unwrap_or_default();
    let stderr = err.join().unwrap_or_default();
    if !status.success() {
        let detail = stderr
            .lines()
            .chain(stdout.lines())
            .map(str::trim)
            .rfind(|line| !line.is_empty() && !line.contains("A complete log of this run"))
            .unwrap_or("no output");
        bail!("{detail}");
    }
    Ok(stdout)
}

/// Pulls the one-time token out of `t3 pair` output.
fn token_from_output(output: &str) -> Option<String> {
    for line in output.lines() {
        let line = line.trim();
        if let Some(token) = line.strip_prefix("Token:") {
            let token = token.trim();
            if !token.is_empty() {
                return Some(token.to_string());
            }
        }
        if let Some(link) = line.strip_prefix("Pairing URL:") {
            let url = url::Url::parse(link.trim()).ok()?;
            let token = url::form_urlencoded::parse(url.fragment()?.as_bytes())
                .find(|(key, _)| key == "token")
                .map(|(_, value)| value.into_owned());
            if token.is_some() {
                return token;
            }
        }
    }
    None
}

fn mint_token(state: &RuntimeState) -> Result<String> {
    const LABEL: &str = "--label YAT3";
    let mut errors = Vec::new();
    let mut scripts = vec![format!(
        "command -v t3 >/dev/null 2>&1 || {{ echo 't3 command not found' >&2; exit 127; }}; exec t3 pair {LABEL}"
    )];
    // The version is a semver string from the server's own state file; keep it that way.
    let version = state
        .server_version
        .as_deref()
        .filter(|v| {
            v.chars()
                .all(|c| c.is_ascii_alphanumeric() || ".-+".contains(c))
        })
        .unwrap_or("latest");
    scripts.push(format!("exec npx --yes t3@{version} pair {LABEL}"));
    for script in scripts {
        match run(shell_command(&script), Duration::from_secs(180)) {
            Ok(output) => match token_from_output(&output) {
                Some(token) => return Ok(token),
                None => errors.push("`t3 pair` printed no token".to_string()),
            },
            Err(error) => errors.push(format!("{error:#}")),
        }
    }
    Err(anyhow!(errors.join("; ")))
}

/// Finds (or opens) T3 Code on this machine and pairs with it.
pub fn pair() -> Result<Connection> {
    let state = match running_server() {
        Some(state) => state,
        None if launch_desktop_app() => {
            wait_for_server(Duration::from_secs(60)).ok_or_else(|| {
                anyhow!("T3 Code opened, but its server did not start within a minute")
            })?
        }
        None => {
            bail!("T3 Code is not running on this machine. Open the T3 Code app or run `t3 serve`")
        }
    };
    let token = mint_token(&state).context("could not create a pairing token")?;
    config::exchange_token(&state.origin, &token)
}

/// True when the saved server is on this machine, so pairing again can be automatic.
pub fn is_local(connection: &Connection) -> bool {
    url::Url::parse(&connection.server_url)
        .ok()
        .and_then(|url| url.host_str().map(str::to_owned))
        .is_some_and(|host| matches!(host.as_str(), "127.0.0.1" | "localhost" | "[::1]" | "::1"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_token_from_pair_output() {
        let output = "Pairing with Mac (http://127.0.0.1:3773).\n\n█▀▀█\n\n\
            Pairing URL: http://127.0.0.1:3773/pair#token=abc%2Bdef\n\
            App link: t3://pair?x\nToken: abc+def\nExpires: 2026-01-01T00:00:00Z\n";
        assert_eq!(token_from_output(output).as_deref(), Some("abc+def"));
        let url_only = "Pairing URL: http://127.0.0.1:3773/pair#token=xyz\n";
        assert_eq!(token_from_output(url_only).as_deref(), Some("xyz"));
        assert_eq!(token_from_output("nothing here"), None);
    }

    #[test]
    fn local_hosts() {
        let mut connection = Connection {
            server_url: "http://127.0.0.1:3773".into(),
            token: String::new(),
            expires_at: None,
        };
        assert!(is_local(&connection));
        connection.server_url = "https://box.tailnet.ts.net".into();
        assert!(!is_local(&connection));
    }
}
