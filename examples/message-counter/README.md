# message-counter

A `message-application` example that subscribes to the `hello-world.events` topic and increments a `messages` counter in the shared `hello-world` KV store on every received message.

## Purpose

Demonstrates cross-module communication via the `messaging` host function. Works as a companion to the [hello-world](../hello-world/README.md) example: each HTTP request to hello-world publishes an event, and this module increments the `messages` counter in response.

## KV Store

Uses the `hello-world` key-value namespace (`spec.keyValue: hello-world`), so its writes are visible to the hello-world module.

| Store | Key | Value |
|---|---|---|
| `counters` | `messages` | `u64` big-endian byte count |

## Trigger

Subscribes to NATS subject `fn.hello-world.events` (set via `spec.topic: hello-world.events`).
