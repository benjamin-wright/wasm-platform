#[allow(clippy::too_many_arguments)]
mod bindings {
    wit_bindgen::generate!({
        world: "http-application",
        path: "../../../framework/runtime.wit",
    });
}

use bindings::{framework, Guest, HttpRequest, HttpResponse};

#[path = "../actions.rs"]
mod actions;

use framework::runtime::{kv, messaging, sql};

struct CancellationHttp;

impl Guest for CancellationHttp {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        let body = if request.method == "GET" {
            if request.query.is_empty() {
                "ready".to_string()
            } else {
                state(&request.query)?
            }
        } else {
            let command = String::from_utf8(request.body.unwrap_or_default())
                .map_err(|_| "invalid command")?;
            if let Some(message) = command.strip_prefix("message ") {
                messaging::send("cancellation-app.events", message.as_bytes())?;
            } else {
                actions::run(&command, "http")?;
            }
            "completed".to_string()
        };
        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "text/plain".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

fn state(token: &str) -> Result<String, String> {
    let rows = sql::query(
        "SELECT COUNT(*) FROM cancellation_effects WHERE token = $1",
        &[sql::Param::Text(token.to_string())],
    )?;
    let count = match rows.first().and_then(|row| row.values.first()) {
        Some(sql::Param::Integer(count)) => *count,
        _ => return Err("invalid SQL count".to_string()),
    };
    let written = kv::get(token)?.is_some();
    Ok(format!("sql={count} kv={written}"))
}

bindings::export!(CancellationHttp with_types_in bindings);
