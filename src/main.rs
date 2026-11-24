mod config;
mod keymap;
mod protocol;
mod rpc;
mod text_input;
mod theme;
mod workspace;

use gpui::{
    App, Application, Bounds, KeyBinding, Menu, MenuItem, TitlebarOptions, WindowBounds,
    WindowOptions, prelude::*, px, size,
};

use crate::keymap::Quit;
use crate::theme::Theme;
use crate::workspace::Workspace;

fn main() {
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("warn")).init();
    let args: Vec<String> = std::env::args().collect();
    // `yat3 pair <link>` pairs from a terminal without opening the window.
    if args.get(1).map(String::as_str) == Some("pair") {
        let Some(link) = args.get(2) else {
            eprintln!("usage: yat3 pair <pairing link>");
            std::process::exit(2);
        };
        match config::pair(link) {
            Ok(connection) => {
                println!(
                    "Paired with {}. Saved to {}",
                    connection.server_url,
                    config::config_file("connection.json").display()
                );
                return;
            }
            Err(error) => {
                eprintln!("Pairing failed: {error:#}");
                std::process::exit(1);
            }
        }
    }

    Application::new().run(|cx: &mut App| {
        let theme = Theme::load();
        let mut notices: Vec<String> = theme.load_error.iter().cloned().collect();
        cx.set_global(theme);
        notices.extend(keymap::load(cx));
        cx.on_action(|_: &Quit, cx| cx.quit());
        cx.set_menus(vec![Menu {
            name: "YAT3".into(),
            items: vec![
                MenuItem::action("Reload Config", keymap::ReloadConfig),
                MenuItem::action("Open Config Folder", keymap::OpenConfigFolder),
                MenuItem::action("Disconnect", keymap::Disconnect),
                MenuItem::separator(),
                MenuItem::action("Quit", Quit),
            ],
        }]);
        // Keep secondary-q working even if keymap.json clears everything else.
        cx.bind_keys([KeyBinding::new("secondary-q", Quit, None)]);

        let bounds = Bounds::centered(None, size(px(1180.), px(780.)), cx);
        let notice = (!notices.is_empty()).then(|| notices.join(" · "));
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                titlebar: Some(TitlebarOptions {
                    title: Some("YAT3".into()),
                    ..Default::default()
                }),
                window_min_size: Some(size(px(640.), px(420.))),
                ..Default::default()
            },
            |window, cx| cx.new(|cx| Workspace::new(notice, window, cx)),
        )
        .expect("open window");
        cx.on_window_closed(|cx| {
            if cx.windows().is_empty() {
                cx.quit();
            }
        })
        .detach();
        cx.activate(true);
    });
}
