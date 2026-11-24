//! Runs and pairs with the T3 Code server on this machine, so nothing else has to be
//! installed.
//!
//! The macOS app bundles the T3 Code server's self-contained build (the same one the `t3` npm
//! package downloads) in `YATT.app/Contents/Resources/t3`. On launch YATT uses a server that is
//! already running, and otherwise starts the bundled one with `t3 serve` and stops it on quit.
//! Both use the usual T3 Code data folder, `~/.t3` (`$T3CODE_HOME` moves it), so threads made
//! with other T3 Code clients show up too.
//!
//! A running server records where it listens in `~/.t3/userdata/server-runtime.json`. The
//! bearer token comes from `t3 auth session issue`, which writes it into that data folder. When
//! a different T3 Code version is running, `t3 pair` from PATH or `npx t3@<its version>` is
//! tried instead.

use std::io::Read as _;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

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
        .context("could not start `t3`")?;
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
            bail!("`t3` did not finish in {} seconds", timeout.as_secs());
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

/// How long tokens issued for YATT last. The server saves them like any other session.
const TOKEN_TTL_DAYS: u64 = 365;

/// The bundled `t3` executable, or `$YATT_T3_BIN`.
pub fn bundled_t3() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("YATT_T3_BIN").filter(|p| !p.is_empty()) {
        return Some(PathBuf::from(path));
    }
    let exe = std::env::current_exe().ok()?;
    let path = exe
        .parent()?
        .parent()?
        .join("Resources")
        .join("t3")
        .join("t3");
    path.is_file().then_some(path)
}

/// The server this process started, stopped by [`stop_server`].
static SERVER: Mutex<Option<Child>> = Mutex::new(None);

fn log_tail(path: &std::path::Path) -> String {
    let text = std::fs::read_to_string(path).unwrap_or_default();
    text.lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .last()
        .unwrap_or("no output")
        .to_string()
}

fn start_bundled_server() -> Result<RuntimeState> {
    let t3 = bundled_t3().ok_or_else(|| {
        anyhow!("no T3 Code server is running, and this build of YATT does not include one")
    })?;
    let dir = config::config_dir();
    std::fs::create_dir_all(&dir).ok();
    let log_path = dir.join("server.log");
    let log = std::fs::File::create(&log_path)
        .with_context(|| format!("creating {}", log_path.display()))?;
    let mut command = Command::new(&t3);
    command
        .arg("serve")
        .stdin(Stdio::null())
        .stdout(log.try_clone()?)
        .stderr(log);
    if let Some(home) = dirs::home_dir() {
        command.current_dir(home);
    }
    let child = command
        .spawn()
        .with_context(|| format!("could not start {}", t3.display()))?;
    *SERVER.lock().unwrap() = Some(child);

    let deadline = Instant::now() + Duration::from_secs(90);
    loop {
        if let Some(state) = running_server() {
            return Ok(state);
        }
        let exited = SERVER
            .lock()
            .unwrap()
            .as_mut()
            .and_then(|child| child.try_wait().ok().flatten());
        if let Some(status) = exited {
            SERVER.lock().unwrap().take();
            bail!(
                "the T3 Code server stopped ({status}): {}",
                log_tail(&log_path)
            );
        }
        if Instant::now() > deadline {
            stop_server();
            bail!(
                "the T3 Code server did not start within 90 seconds: {}",
                log_tail(&log_path)
            );
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// Uses the server that is already running, or starts the bundled one.
pub fn ensure_server() -> Result<RuntimeState> {
    if let Some(state) = running_server() {
        return Ok(state);
    }
    start_bundled_server()
}

/// Stops the server this process started, if any. A server started elsewhere keeps running.
pub fn stop_server() {
    let Some(mut child) = SERVER.lock().unwrap().take() else {
        return;
    };
    // SIGTERM first so the server closes its database and its agent processes.
    let _ = Command::new("kill")
        .args(["-TERM", &child.id().to_string()])
        .status();
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if child.try_wait().ok().flatten().is_some() {
            return;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    let _ = child.kill();
    let _ = child.wait();
}

fn issue_with_bundled() -> Result<String> {
    let t3 = bundled_t3().ok_or_else(|| anyhow!("no bundled T3 Code server"))?;
    let mut command = Command::new(t3);
    command.args([
        "auth",
        "session",
        "issue",
        "--token-only",
        "--label",
        "YATT",
        "--ttl",
        &format!("{TOKEN_TTL_DAYS}d"),
    ]);
    let output = run(command, Duration::from_secs(60))?;
    output
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .last()
        .map(str::to_owned)
        .ok_or_else(|| anyhow!("`t3 auth session issue` printed no token"))
}

fn pair_with_cli(state: &RuntimeState) -> Result<String> {
    const LABEL: &str = "--label YATT";
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

/// Makes sure a local server runs and gets a token for it.
pub fn pair() -> Result<Connection> {
    let state = ensure_server()?;
    let bundled_error = match issue_with_bundled() {
        Ok(token) => {
            let expires_at = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .ok()
                .map(|now| now.as_secs() + TOKEN_TTL_DAYS * 86_400);
            let connection = Connection {
                server_url: state.origin.trim_end_matches('/').to_string(),
                token,
                expires_at,
            };
            config::save_connection(&connection)?;
            return Ok(connection);
        }
        Err(error) => error,
    };
    let token = pair_with_cli(&state)
        .map_err(|error| anyhow!("could not get a token ({bundled_error:#}; {error:#})"))?;
    config::exchange_token(&state.origin, &token)
}

/// Starts the local server if needed before using a saved connection to it, and follows the
/// server to a new port. Returns the connection to use.
pub fn prepare(mut connection: Connection) -> Result<Connection> {
    // T3_SERVER_URL points at a server someone else runs, such as scripts/fake-server.py.
    if !is_local(&connection) || std::env::var_os("T3_SERVER_URL").is_some() {
        return Ok(connection);
    }
    let state = ensure_server()?;
    let origin = state.origin.trim_end_matches('/');
    if connection.server_url != origin {
        connection.server_url = origin.to_string();
        config::save_connection(&connection)?;
    }
    Ok(connection)
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
