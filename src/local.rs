//! Runs and pairs with the YATT server on this machine, so nothing else has to be installed.
//!
//! The macOS app bundles `yatt-server`, the Go port of the T3 Code server, in
//! `YATT.app/Contents/Resources/yatt-server`. On launch YATT uses a server that is already
//! running, and otherwise starts the bundled one with `yatt-server serve` and stops it on quit.
//! Its data lives in `~/.yatt/server` (`$YATT_SERVER_HOME` moves it), apart from T3 Code's
//! `~/.t3`, so the two servers never share a database.
//!
//! A running server records where it listens in `~/.yatt/server/userdata/server-runtime.json`.
//! The bearer token comes from `yatt-server auth session issue`, which writes it into that data
//! folder.

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
}

fn server_home() -> Option<PathBuf> {
    match std::env::var_os("YATT_SERVER_HOME") {
        Some(home) if !home.is_empty() => Some(PathBuf::from(home)),
        _ => dirs::home_dir().map(|home| home.join(".yatt").join("server")),
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
    let path = server_home()?.join("userdata").join("server-runtime.json");
    let text = std::fs::read_to_string(path).ok()?;
    let state: RuntimeState = serde_json::from_str(&text).ok()?;
    probe(&state.origin).then_some(state)
}

fn run(mut command: Command, timeout: Duration) -> Result<String> {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("could not start `yatt-server`")?;
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
            bail!(
                "`yatt-server` did not finish in {} seconds",
                timeout.as_secs()
            );
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
            .rfind(|line| !line.is_empty())
            .unwrap_or("no output");
        bail!("{detail}");
    }
    Ok(stdout)
}

/// How long tokens issued for YATT last. The server saves them like any other session.
const TOKEN_TTL_DAYS: u64 = 365;

/// The bundled `yatt-server` executable, or `$YATT_SERVER_BIN`.
pub fn bundled_server() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("YATT_SERVER_BIN").filter(|p| !p.is_empty()) {
        return Some(PathBuf::from(path));
    }
    let exe = std::env::current_exe().ok()?;
    let path = exe
        .parent()?
        .parent()?
        .join("Resources")
        .join("yatt-server");
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
    let server = bundled_server().ok_or_else(|| {
        anyhow!("no YATT server is running, and this build of YATT does not include one")
    })?;
    let dir = config::config_dir();
    std::fs::create_dir_all(&dir).ok();
    let log_path = dir.join("server.log");
    let log = std::fs::File::create(&log_path)
        .with_context(|| format!("creating {}", log_path.display()))?;
    let mut command = Command::new(&server);
    // Any free port: the runtime state file says which one it took.
    command
        .args(["serve", "--port", "0"])
        .stdin(Stdio::null())
        .stdout(log.try_clone()?)
        .stderr(log);
    if let Some(home) = dirs::home_dir() {
        command.current_dir(home);
    }
    let child = command
        .spawn()
        .with_context(|| format!("could not start {}", server.display()))?;
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
                "the YATT server stopped ({status}): {}",
                log_tail(&log_path)
            );
        }
        if Instant::now() > deadline {
            stop_server();
            bail!(
                "the YATT server did not start within 90 seconds: {}",
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

fn issue_token() -> Result<String> {
    let server = bundled_server().ok_or_else(|| anyhow!("this build of YATT has no server"))?;
    let mut command = Command::new(server);
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
        .ok_or_else(|| anyhow!("`yatt-server auth session issue` printed no token"))
}

/// Makes sure a local server runs and gets a token for it.
pub fn pair() -> Result<Connection> {
    let state = ensure_server()?;
    let token = issue_token().context("could not get a token")?;
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
    Ok(connection)
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
