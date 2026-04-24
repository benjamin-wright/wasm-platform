wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

use framework::runtime::{log, sql};

struct SqlHelloQuery;

impl Guest for SqlHelloQuery {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "sql-hello: query");

        let rows = sql::query(
            "SELECT id, name FROM greetings WHERE active = $1",
            &[sql::Param::Boolean(true)],
        )?;

        let mut items = Vec::with_capacity(rows.len());
        for row in &rows {
            let id = match row.values.get(0) {
                Some(sql::Param::Integer(n)) => *n,
                Some(sql::Param::Null) => return Err("null id".to_string()),
                _ => return Err("unexpected type for id column".to_string()),
            };
            let name = match row.values.get(1) {
                Some(sql::Param::Text(s)) => s.clone(),
                Some(sql::Param::Null) => return Err("null name".to_string()),
                _ => return Err("unexpected type for name column".to_string()),
            };
            items.push(format!("{{\"id\":{id},\"name\":{name:?}}}"));
        }

        let body = format!("[{}]", items.join(","));

        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "application/json".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(SqlHelloQuery);
