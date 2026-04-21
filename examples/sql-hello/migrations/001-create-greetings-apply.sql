CREATE TABLE IF NOT EXISTS greetings (
    id     serial      PRIMARY KEY,
    name   text        NOT NULL UNIQUE,
    active bool        NOT NULL DEFAULT true
);
