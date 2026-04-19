use sqlx::{Column, Row, TypeInfo};
use sqlx::postgres::PgRow;
use tokio::runtime::Handle;

use crate::runtime::{HostState, message_bindings};

type Param = message_bindings::framework::runtime::sql::Param;
type SqlRow = message_bindings::framework::runtime::sql::Row;
type PgQuery<'q> = sqlx::query::Query<'q, sqlx::Postgres, sqlx::postgres::PgArguments>;

impl message_bindings::framework::runtime::sql::Host for HostState {
    fn query(&mut self, sql: String, params: Vec<Param>) -> Result<Vec<SqlRow>, String> {
        let pool = match &self.sql_pool {
            Some(p) => p.clone(),
            None => {
                return Err(
                    "SQL host function unavailable: function has no sql user".to_string(),
                )
            }
        };
        Handle::current().block_on(async move {
            let mut q = sqlx::query(&sql);
            for p in &params {
                q = bind_param(q, p);
            }
            let rows = q.fetch_all(&pool).await.map_err(|e| e.to_string())?;
            rows.iter().map(decode_row).collect()
        })
    }

    fn execute(&mut self, sql: String, params: Vec<Param>) -> Result<u64, String> {
        let pool = match &self.sql_pool {
            Some(p) => p.clone(),
            None => {
                return Err(
                    "SQL host function unavailable: function has no sql user".to_string(),
                )
            }
        };
        Handle::current().block_on(async move {
            let mut q = sqlx::query(&sql);
            for p in &params {
                q = bind_param(q, p);
            }
            let result = q.execute(&pool).await.map_err(|e| e.to_string())?;
            Ok(result.rows_affected())
        })
    }
}

fn bind_param<'q>(q: PgQuery<'q>, param: &Param) -> PgQuery<'q> {
    match param {
        Param::Null => q.bind(Option::<String>::None),
        Param::Boolean(b) => q.bind(*b),
        Param::Integer(i) => q.bind(*i),
        Param::Real(f) => q.bind(*f),
        Param::Text(s) => q.bind(s.clone()),
        Param::Blob(b) => q.bind(b.clone()),
    }
}

fn decode_row(row: &PgRow) -> Result<SqlRow, String> {
    let columns = row.columns();
    let mut names = Vec::with_capacity(columns.len());
    let mut values = Vec::with_capacity(columns.len());
    for (i, col) in columns.iter().enumerate() {
        names.push(col.name().to_owned());
        values.push(decode_column(row, i, col.type_info().name())?);
    }
    Ok(SqlRow { columns: names, values })
}

fn decode_column(row: &PgRow, i: usize, type_name: &str) -> Result<Param, String> {
    match type_name {
        "BOOL" => {
            let v: Option<bool> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(Param::Boolean).unwrap_or(Param::Null))
        }
        "INT2" => {
            let v: Option<i16> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(|n| Param::Integer(n as i64)).unwrap_or(Param::Null))
        }
        "INT4" => {
            let v: Option<i32> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(|n| Param::Integer(n as i64)).unwrap_or(Param::Null))
        }
        "INT8" => {
            let v: Option<i64> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(Param::Integer).unwrap_or(Param::Null))
        }
        "FLOAT4" => {
            let v: Option<f32> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(|f| Param::Real(f64::from(f))).unwrap_or(Param::Null))
        }
        "FLOAT8" => {
            let v: Option<f64> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(Param::Real).unwrap_or(Param::Null))
        }
        "TEXT" | "VARCHAR" | "BPCHAR" | "NAME" => {
            let v: Option<String> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(Param::Text).unwrap_or(Param::Null))
        }
        "BYTEA" => {
            let v: Option<Vec<u8>> = row.try_get(i).map_err(|e| e.to_string())?;
            Ok(v.map(Param::Blob).unwrap_or(Param::Null))
        }
        other => Err(format!("unsupported column type: {other}")),
    }
}
