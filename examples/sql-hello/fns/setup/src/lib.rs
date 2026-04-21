wit_bindgen::generate!({
    world: "http-application",
    path: "../../../../framework/runtime.wit",
});

use framework::runtime::{log, sql};

struct SqlHelloSetup;

impl Guest for SqlHelloSetup {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "sql-hello: setup");

        sql::execute(
            "CREATE TABLE IF NOT EXISTS greetings (\
                id     serial PRIMARY KEY,\
                name   text   NOT NULL UNIQUE,\
                active bool   NOT NULL DEFAULT true\
            )",
            &[],
        )?;

        sql::execute(
            "INSERT INTO greetings (name, active) VALUES \
                ('Alice', true), ('Bob', true), ('Carol', false) \
             ON CONFLICT (name) DO UPDATE SET active = EXCLUDED.active",
            &[],
        )?;

        Ok(HttpResponse {
            status: 200,
            headers: vec![],
            body: None,
        })
    }
}

export!(SqlHelloSetup);
