wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

use framework::runtime::{log, sql};

struct SqlHelloInsertTest;

impl Guest for SqlHelloInsertTest {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "sql-hello: insert-test");

        match sql::execute(
            "INSERT INTO greetings (name, active) VALUES ('TestUser', true)",
            &[],
        ) {
            Ok(_) => Ok(HttpResponse {
                status: 200,
                headers: vec![],
                body: Some(b"unexpected success".to_vec()),
            }),
            Err(e) if e.to_lowercase().contains("permission denied") => Ok(HttpResponse {
                status: 403,
                headers: vec![],
                body: Some(b"permission denied".to_vec()),
            }),
            Err(e) => Ok(HttpResponse {
                status: 500,
                headers: vec![],
                body: Some(e.into_bytes()),
            }),
        }
    }}

export!(SqlHelloInsertTest);
