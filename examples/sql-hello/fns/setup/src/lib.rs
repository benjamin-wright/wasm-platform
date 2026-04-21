wit_bindgen::generate!({
    world: "http-application",
    path: "../../../../framework/runtime.wit",
});

use framework::runtime::{log, sql};

struct SqlHelloSetup;

impl Guest for SqlHelloSetup {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "sql-hello: setup");

        // The greetings table is created by the migrations Job before this function
        // is activated. This handler only seeds/refreshes the rows.
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
