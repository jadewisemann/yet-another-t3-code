//! Files this client keeps in its config folder:
//!
//! - `connection.json`: the T3 Code server URL and the bearer token from pairing.
//! - `keymap.json`: user key bindings, layered over the defaults.
//! - `theme.json`: color overrides, layered over the built-in theme.

use std::path::PathBuf;

use anyhow::{Context as _, Result, anyhow, bail};
use serde::{Deserialize, Serialize};

pub fn config_dir() -> PathBuf {
    if let Some(dir) = std::env::var_os("YAT3_CONFIG_DIR") {
        return PathBuf::from(dir);
    }
    dirs::config_dir()
        .or_else(|| dirs::home_dir().map(|home| home.join(".config")))
        .unwrap_or_else(|| PathBuf::from("."))
        .join("yat3")
}

pub fn config_file(name: &str) -> PathBuf {
    config_dir().join(name)
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Connection {
    /// Origin of the T3 Code server, such as `http://127.0.0.1:3773`.
    pub server_url: String,
    pub token: String,
    /// Unix seconds when the token stops working, if the server said.
    #[serde(default)]
    pub expires_at: Option<u64>,
}

impl Connection {
    pub fn websocket_url(&self) -> Result<String> {
        let mut url = url::Url::parse(&self.server_url).context("invalid server URL")?;
        let scheme = match url.scheme() {
            "https" | "wss" => "wss",
            _ => "ws",
        };
        url.set_scheme(scheme)
            .map_err(|_| anyhow!("cannot use {scheme} with this server URL"))?;
        url.set_path("/ws");
        url.set_fragment(None);
        url.query_pairs_mut()
            .clear()
            // The server refuses sockets from clients built for another orchestration protocol.
            .append_pair(
                "orchestrationProtocol",
                crate::protocol::ORCHESTRATION_PROTOCOL_VERSION,
            )
            .append_pair("clientSurface", "desktop")
            .append_pair("clientAppVersion", env!("CARGO_PKG_VERSION"));
        Ok(url.to_string())
    }
}

/// Environment variables win over the saved file, which helps when trying another server.
pub fn load_connection() -> Option<Connection> {
    if let (Ok(server_url), Ok(token)) = (std::env::var("T3_SERVER_URL"), std::env::var("T3_TOKEN"))
    {
        return Some(Connection {
            server_url,
            token,
            expires_at: None,
        });
    }
    let text = std::fs::read_to_string(config_file("connection.json")).ok()?;
    serde_json::from_str(&text).ok()
}

pub fn save_connection(connection: &Connection) -> Result<()> {
    let dir = config_dir();
    std::fs::create_dir_all(&dir).with_context(|| format!("creating {}", dir.display()))?;
    let path = config_file("connection.json");
    std::fs::write(&path, serde_json::to_string_pretty(connection)?)
        .with_context(|| format!("writing {}", path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600));
    }
    Ok(())
}

pub fn forget_connection() {
    let _ = std::fs::remove_file(config_file("connection.json"));
}

const TOKEN_EXCHANGE_GRANT: &str = "urn:ietf:params:oauth:grant-type:token-exchange";
const BOOTSTRAP_TOKEN_TYPE: &str = "urn:t3:params:oauth:token-type:environment-bootstrap";
const ACCESS_TOKEN_TYPE: &str = "urn:ietf:params:oauth:token-type:access_token";

#[derive(Deserialize)]
struct AccessTokenResult {
    access_token: String,
    #[serde(default)]
    expires_in: Option<f64>,
}

/// Exchanges a pairing link (`http://host:port/pair#token=...`, from `t3 pair` or the
/// desktop app's pairing screen) for a bearer token, the same way the web client does.
pub fn pair(pairing_url: &str) -> Result<Connection> {
    let url = url::Url::parse(pairing_url.trim()).context("this does not look like a URL")?;
    let token = url
        .fragment()
        .and_then(|fragment| {
            url::form_urlencoded::parse(fragment.as_bytes())
                .find(|(key, _)| key == "token")
                .map(|(_, value)| value.into_owned())
        })
        .or_else(|| {
            url.query_pairs()
                .find(|(key, _)| key == "token")
                .map(|(_, value)| value.into_owned())
        })
        .ok_or_else(|| anyhow!("the link has no token. Copy the whole pairing link."))?;
    let origin = url.origin();
    if !origin.is_tuple() {
        bail!("the link has no server address");
    }
    exchange_token(&origin.ascii_serialization(), &token)
}

/// Trades a one-time pairing token for a bearer token at `server_url` and saves it.
pub fn exchange_token(server_url: &str, token: &str) -> Result<Connection> {
    let server_url = server_url.trim_end_matches('/').to_string();
    let os = std::env::consts::OS;
    let response = ureq::post(format!("{server_url}/oauth/token")).send_form([
        ("grant_type", TOKEN_EXCHANGE_GRANT),
        ("subject_token", token),
        ("subject_token_type", BOOTSTRAP_TOKEN_TYPE),
        ("requested_token_type", ACCESS_TOKEN_TYPE),
        ("client_label", "YAT3 (GPUI)"),
        ("client_os", os),
    ]);
    let mut response = match response {
        Ok(response) => response,
        Err(ureq::Error::StatusCode(status)) => {
            bail!("the server refused the pairing link (HTTP {status}). It may be used or expired.")
        }
        Err(error) => return Err(anyhow!(error).context("could not reach the server")),
    };
    let result: AccessTokenResult = response
        .body_mut()
        .read_json()
        .context("unexpected token response")?;
    let expires_at = result.expires_in.and_then(|seconds| {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .ok()
            .map(|now| now.as_secs() + seconds as u64)
    });
    let connection = Connection {
        server_url,
        token: result.access_token,
        expires_at,
    };
    save_connection(&connection)?;
    Ok(connection)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn websocket_url_keeps_host_and_adds_protocol() {
        let connection = Connection {
            server_url: "http://127.0.0.1:3773".into(),
            token: "t".into(),
            expires_at: None,
        };
        let url = connection.websocket_url().unwrap();
        assert!(url.starts_with("ws://127.0.0.1:3773/ws?orchestrationProtocol=2"));

        let secure = Connection {
            server_url: "https://box.tailnet.ts.net".into(),
            ..connection
        };
        assert!(
            secure
                .websocket_url()
                .unwrap()
                .starts_with("wss://box.tailnet.ts.net/ws?")
        );
    }
}
