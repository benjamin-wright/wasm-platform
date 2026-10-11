use std::time::Duration;

use sqlparser::{ast::Statement, dialect::PostgreSqlDialect, parser::Parser};
use sqlx::postgres::{PgConnection, PgPool, PgRow};
use sqlx::{Column, Connection, Row, TypeInfo};

use crate::invocation::Invocation;
use crate::runtime::{HostState, message_bindings};

type Param = message_bindings::framework::runtime::sql::Param;
type SqlRow = message_bindings::framework::runtime::sql::Row;
type PgQuery<'q> = sqlx::query::Query<'q, sqlx::Postgres, sqlx::postgres::PgArguments>;

const SQL_CLEANUP_TIMEOUT: Duration = Duration::from_secs(2);
const SQL_TERMINATION_WAIT_MS: i64 = 1_000;

enum SqlOperation {
    Query,
    Execute,
}

enum SqlOutput {
    Rows(Vec<PgRow>),
    Affected(u64),
}

impl message_bindings::framework::runtime::sql::Host for HostState {
    async fn query(&mut self, sql: String, params: Vec<Param>) -> Result<Vec<SqlRow>, String> {
        self.invocation.check()?;
        let pool = match &self.sql_pool {
            Some(p) => p.clone(),
            None => {
                return Err("SQL host function unavailable: function has no sql user".to_string());
            }
        };
        match run_sql(
            self.invocation.clone(),
            pool,
            sql,
            params,
            SqlOperation::Query,
        )
        .await?
        {
            SqlOutput::Rows(rows) => rows
                .iter()
                .map(|row| {
                    self.invocation.check()?;
                    decode_row(row)
                })
                .collect(),
            SqlOutput::Affected(_) => Err("SQL query returned an invalid result".to_string()),
        }
    }

    async fn execute(&mut self, sql: String, params: Vec<Param>) -> Result<u64, String> {
        self.invocation.check()?;
        let pool = match &self.sql_pool {
            Some(p) => p.clone(),
            None => {
                return Err("SQL host function unavailable: function has no sql user".to_string());
            }
        };
        match run_sql(
            self.invocation.clone(),
            pool,
            sql,
            params,
            SqlOperation::Execute,
        )
        .await?
        {
            SqlOutput::Affected(affected) => Ok(affected),
            SqlOutput::Rows(_) => Err("SQL execute returned an invalid result".to_string()),
        }
    }
}

async fn run_sql(
    invocation: Invocation,
    pool: PgPool,
    sql: String,
    params: Vec<Param>,
    operation: SqlOperation,
) -> Result<SqlOutput, String> {
    invocation.check()?;
    validate_statement(&sql)?;
    let (result_tx, result_rx) = tokio::sync::oneshot::channel();
    let worker_invocation = invocation.clone();
    let task = tokio::spawn(async move {
        let result = sql_job(worker_invocation, pool, sql, params, operation).await;
        let _ = result_tx.send(result);
    });
    invocation.track(task);
    invocation
        .run(result_rx)
        .await?
        .map_err(|_| "SQL operation task failed".to_string())?
}

async fn sql_job(
    invocation: Invocation,
    pool: PgPool,
    sql: String,
    params: Vec<Param>,
    operation: SqlOperation,
) -> Result<SqlOutput, String> {
    let mut connection = invocation
        .run(pool.acquire())
        .await?
        .map_err(|error| error.to_string())?;
    connection.close_on_drop();

    let mut backend_pid = None;
    let mut commit_submitted = false;
    let result = async {
        invocation.check()?;
        invocation
            .run(sqlx::query("BEGIN").execute(&mut *connection))
            .await?
            .map_err(|error| error.to_string())?;
        let timeout_ms = invocation
            .deadline()
            .saturating_duration_since(tokio::time::Instant::now())
            .as_millis()
            .clamp(1, i32::MAX as u128);
        let setup = invocation
            .run(
                sqlx::query("SELECT pg_backend_pid(), set_config('statement_timeout', $1, true)")
                    .bind(format!("{timeout_ms}ms"))
                    .fetch_one(&mut *connection),
            )
            .await?
            .map_err(|error| error.to_string())?;
        backend_pid = Some(
            setup
                .try_get::<i32, _>(0)
                .map_err(|error| error.to_string())?,
        );

        invocation.check()?;
        let mut query = sqlx::query(&sql);
        for param in &params {
            query = bind_param(query, param);
        }
        let output = invocation
            .run(async {
                match operation {
                    SqlOperation::Query => {
                        query.fetch_all(&mut *connection).await.map(SqlOutput::Rows)
                    }
                    SqlOperation::Execute => query
                        .execute(&mut *connection)
                        .await
                        .map(|result| SqlOutput::Affected(result.rows_affected())),
                }
            })
            .await?
            .map_err(|error| error.to_string())?;
        invocation.check()?;
        invocation
            .run(async {
                commit_submitted = true;
                sqlx::query("COMMIT").execute(&mut *connection).await
            })
            .await?
            .map_err(|error| error.to_string())?;
        Ok(output)
    }
    .await;

    let result = if backend_pid.is_some() && invocation.check().is_err() {
        if commit_submitted {
            tracing::warn!("SQL cancellation after COMMIT submission; commit outcome is uncertain");
        }
        match backend_pid {
            Some(pid) if terminate_backend(&pool, pid).await => result,
            _ => {
                tracing::error!(
                    commit_submitted,
                    "SQL cancellation could not confirm backend termination"
                );
                Err(
                    "invocation cancelled: SQL backend termination could not be confirmed"
                        .to_string(),
                )
            }
        }
    } else {
        if result.is_err() {
            let _ = invocation
                .run(sqlx::query("ROLLBACK").execute(&mut *connection))
                .await;
        }
        result
    };
    // The tracked job owns connection cleanup even when the guest future is dropped.
    let _ = tokio::time::timeout(SQL_CLEANUP_TIMEOUT, connection.close()).await;
    result
}

fn validate_statement(sql: &str) -> Result<(), String> {
    let statements = Parser::parse_sql(&PostgreSqlDialect {}, sql)
        .map_err(|_| "SQL statement could not be parsed".to_string())?;
    match statements.as_slice() {
        [
            Statement::Query(_)
            | Statement::Insert(_)
            | Statement::Update(_)
            | Statement::Delete(_)
            | Statement::CreateTable(_)
            | Statement::CreateIndex(_)
            | Statement::AlterTable(_)
            | Statement::Drop { .. }
            | Statement::Truncate(_),
        ] => Ok(()),
        _ => Err("SQL statement is not supported by invocation-owned transactions".to_string()),
    }
}

async fn terminate_backend(pool: &PgPool, backend_pid: i32) -> bool {
    let cleanup = async {
        let mut connection = PgConnection::connect_with(&pool.connect_options()).await?;
        let result = sqlx::query_scalar::<_, bool>("SELECT pg_terminate_backend($1, $2)")
            .bind(backend_pid)
            .bind(SQL_TERMINATION_WAIT_MS)
            .fetch_one(&mut connection)
            .await;
        let _ = connection.close().await;
        result
    };
    matches!(
        tokio::time::timeout(SQL_CLEANUP_TIMEOUT, cleanup).await,
        Ok(Ok(true))
    )
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
    Ok(SqlRow {
        columns: names,
        values,
    })
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ordinary_statements_use_host_owned_transactions() {
        for sql in [
            "SELECT 'COMMIT' AS value",
            "/* outer /* BEGIN */ */ SELECT 1",
            "INSERT INTO events(id) VALUES ($1)",
            "UPDATE events SET id = $1 WHERE id = $2",
            "DELETE FROM events WHERE id = $1",
            "WITH inserted AS (INSERT INTO events(id) VALUES (1) RETURNING id) SELECT id FROM inserted",
            "CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY)",
            "CREATE INDEX events_idx ON events(id)",
            "ALTER TABLE events ADD COLUMN value BIGINT",
            "DROP TABLE events",
            "TRUNCATE TABLE events",
        ] {
            assert!(validate_statement(sql).is_ok(), "rejected: {sql}");
        }
    }

    #[test]
    fn guest_cannot_commit_or_run_transaction_controlling_procedures() {
        for sql in [
            "BEGIN",
            "START TRANSACTION",
            "COMMIT",
            "END",
            "ABORT",
            "ROLLBACK",
            "SAVEPOINT guest",
            "RELEASE SAVEPOINT guest",
            "PREPARE TRANSACTION 'guest'",
            "COMMIT PREPARED 'guest'",
            "PREPARE guest AS SELECT 1",
            "EXECUTE guest",
            "SET statement_timeout = 0",
            "CALL guest()",
            "DO $$ BEGIN COMMIT; END $$",
            "SELECT 1; COMMIT",
            "",
            ";",
        ] {
            assert!(validate_statement(sql).is_err(), "accepted: {sql}");
        }
    }

    #[tokio::test]
    async fn cancelled_sql_never_starts_a_worker_or_connection() {
        let pool = sqlx::postgres::PgPoolOptions::new()
            .connect_lazy("postgres://localhost:1/test")
            .unwrap();
        let invocation = Invocation::new(Duration::from_secs(5));
        invocation.cancel();
        assert!(
            run_sql(
                invocation.clone(),
                pool.clone(),
                "INSERT INTO events(id) VALUES (1)".to_string(),
                Vec::new(),
                SqlOperation::Execute,
            )
            .await
            .is_err()
        );
        invocation.finish().await;
        assert_eq!(pool.size(), 0);
    }
}
