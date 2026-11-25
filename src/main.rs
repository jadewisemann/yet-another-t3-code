mod config;
mod keymap;
mod local;
mod protocol;
mod rpc;
mod terminal;
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
    // `yatt pair [link]` pairs from a terminal without opening the window. Without a link
    // it pairs with T3 Code on this machine.
    if args.get(1).map(String::as_str) == Some("pair") {
        let result = match args.get(2) {
            Some(link) => config::pair(link),
            None => local::pair(),
        }
        .and_then(|connection| {
            rpc::check(&connection).map_err(|error| error.context("the new token did not work"))?;
            Ok(connection)
        });
        // A server started just for pairing stops with this command.
        local::stop_server();
        match result {
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
        // Stop the bundled server however the app quits (menu, last window, logout).
        cx.on_app_quit(|_| async { local::stop_server() }).detach();
        cx.set_menus(vec![Menu {
            name: "YATT".into(),
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
                    title: Some("YATT".into()),
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
