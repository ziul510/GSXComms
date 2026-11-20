// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

use serde::{Deserialize, Serialize};
use simconnect::{
    SimConnect,
    proto::{
        codec::PacketReader,
        enums::{DataRequestFlags, DataSetFlags, DataType, Period, RecvId},
        recv::{parse_exception, parse_sim_object_data},
    },
};
use std::{
    collections::HashMap,
    io::{self, BufRead, Write},
    sync::Arc,
    time::Duration,
};
use tokio::{sync::mpsc, time::timeout};

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Request {
    Get { name: String, timeout_ms: u64 },
    Set { name: String, value: f64 },
}

#[derive(Default, Serialize)]
struct Reply {
    #[serde(skip_serializing_if = "Option::is_none")]
    value: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    fatal: bool,
}

enum Message {
    Value(u32, u32, f64),
    Exception(u32, String),
    Disconnected(String),
}

fn units(name: &str) -> Option<&'static str> {
    match name {
        "GPS GROUND SPEED" | "GROUND VELOCITY" => Some("knots"),
        "PLANE LATITUDE" | "PLANE LONGITUDE" => Some("degrees"),
        "PLANE ALTITUDE" => Some("feet"),
        "L:FSDT_GSX_COUATL_STARTED"
        | "L:FSDT_GSX_SET_REMOTECONTROL"
        | "L:FSDT_GSX_MENU_OPEN"
        | "L:FSDT_GSX_MENU_CHOICE" => Some("number"),
        _ => None,
    }
}

fn send_reply(reply: Reply) -> io::Result<()> {
    let mut out = io::stdout().lock();
    serde_json::to_writer(&mut out, &reply)?;
    writeln!(out)?;
    out.flush()
}

fn parse_message(packet: &[u8]) -> Option<Message> {
    let mut reader = PacketReader::new(packet);
    let header = reader.header().ok()?;
    match header.id {
        id if id == RecvId::SimObjectData as u32 => {
            let data = parse_sim_object_data(&mut reader).ok()?;
            let bytes = data.data.get(..8)?.try_into().ok()?;
            Some(Message::Value(
                data.request_id,
                data.define_id,
                f64::from_le_bytes(bytes),
            ))
        }
        id if id == RecvId::Exception as u32 => {
            let error = parse_exception(&mut reader).ok()?;
            Some(Message::Exception(
                error.send_id,
                format!("{:?}", error.exception),
            ))
        }
        id if id == RecvId::Quit as u32 => {
            Some(Message::Disconnected("Simulator closed SimConnect".into()))
        }
        _ => None,
    }
}

async fn receive(sim: Arc<SimConnect>, messages: mpsc::Sender<Message>) {
    loop {
        match sim.recv_ref().await {
            Ok(packet) => {
                let message = parse_message(&packet);
                drop(packet);
                if let Some(message) = message {
                    let disconnected = matches!(message, Message::Disconnected(_));
                    if messages.send(message).await.is_err() || disconnected {
                        break;
                    }
                }
            }
            Err(error) => {
                let _ = messages
                    .send(Message::Disconnected(error.to_string()))
                    .await;
                break;
            }
        }
    }
}

async fn wait_for_reply(
    messages: &mut mpsc::Receiver<Message>,
    definition: u32,
    registered: u32,
    sent: u32,
    request_id: Option<u32>,
    wait: Duration,
) -> Reply {
    let result = timeout(wait, async {
        loop {
            // A late response can arrive during the next request. Match reads by both IDs
            // and match exceptions by their original SimConnect send ID.
            match messages.recv().await {
                Some(Message::Value(id, def, value))
                    if Some(id) == request_id && def == definition =>
                {
                    return if value.is_finite() {
                        Reply {
                            value: Some(value),
                            ..Reply::default()
                        }
                    } else {
                        error("Simulator returned a non-finite value", false)
                    };
                }
                Some(Message::Exception(id, reason)) if id == registered => {
                    return error(format!("SimConnect definition failed: {reason}"), true);
                }
                Some(Message::Exception(id, reason)) if id == sent => {
                    return error(format!("SimConnect request failed: {reason}"), false);
                }
                Some(Message::Disconnected(reason)) => return error(reason, true),
                None => return error("SimConnect reader exited", true),
                _ => {}
            }
        }
    })
    .await;
    match result {
        Ok(reply) => reply,
        Err(_) if request_id.is_some() => error("SimConnect read timed out", false),
        // The wire protocol only acknowledges failed writes, so allow a short exception window.
        Err(_) => Reply::default(),
    }
}

fn error(message: impl Into<String>, fatal: bool) -> Reply {
    Reply {
        error: Some(message.into()),
        fatal,
        ..Reply::default()
    }
}

async fn request(
    request: Request,
    sim: &SimConnect,
    definitions: &mut HashMap<String, (u32, u32)>,
    next_id: &mut u32,
    messages: &mut mpsc::Receiver<Message>,
) -> Reply {
    let name = match &request {
        Request::Get { name, .. } | Request::Set { name, .. } => name,
    };
    let Some(unit) = units(name) else {
        return error("Unsupported simulator variable", false);
    };
    let (definition, registered) = match definitions.get(name) {
        Some(ids) => *ids,
        None => {
            // Keep a variable's definition and the registration send ID for this connection.
            let definition = definitions.len() as u32 + 1;
            match sim
                .add_to_data_definition(definition, name, Some(unit), DataType::Float64, 0.0, -1)
                .await
            {
                Ok(id) => {
                    definitions.insert(name.clone(), (definition, id));
                    (definition, id)
                }
                Err(reason) => return error(reason.to_string(), true),
            }
        }
    };
    let (sent, id, wait) = match request {
        Request::Get { timeout_ms, .. } => {
            *next_id = next_id.wrapping_add(1);
            (
                sim.request_data_on_sim_object(
                    *next_id,
                    definition,
                    0,
                    Period::Once,
                    DataRequestFlags::empty(),
                    0,
                    0,
                    0,
                )
                .await,
                Some(*next_id),
                Duration::from_millis(timeout_ms.clamp(1, 850)),
            )
        }
        Request::Set { value, .. } => {
            if !value.is_finite()
                || !matches!(
                    name.as_str(),
                    "L:FSDT_GSX_SET_REMOTECONTROL"
                        | "L:FSDT_GSX_MENU_OPEN"
                        | "L:FSDT_GSX_MENU_CHOICE"
                )
            {
                return error("Unsupported simulator write", false);
            }
            (
                sim.set_data_on_sim_object(
                    definition,
                    0,
                    DataSetFlags::DEFAULT,
                    0,
                    8,
                    &value.to_le_bytes(),
                )
                .await,
                None,
                Duration::from_millis(45),
            )
        }
    };
    match sent {
        Ok(sent) => wait_for_reply(messages, definition, registered, sent, id, wait).await,
        Err(reason) => error(reason.to_string(), true),
    }
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> io::Result<()> {
    let sim = match SimConnect::open_local("GSXComms").await {
        Ok(sim) => Arc::new(sim),
        Err(reason) => return send_reply(error(reason.to_string(), true)),
    };
    send_reply(Reply::default())?;
    // Read the parent's pipe off the async runtime so a quiet pipe cannot block SimConnect.
    let (tx, mut input) = mpsc::channel(1);
    std::thread::spawn(move || {
        for line in io::stdin().lock().lines().map_while(Result::ok) {
            if tx.blocking_send(line).is_err() {
                break;
            }
        }
    });
    let (tx, mut messages) = mpsc::channel(16);
    tokio::spawn(receive(Arc::clone(&sim), tx));
    let mut definitions = HashMap::new();
    let mut next_id = 0u32;
    // The parent submits one request at a time. EOF or a fatal reply ends this connection.
    while let Some(line) = input.recv().await {
        let reply = match serde_json::from_str(&line) {
            Ok(req) => request(req, &sim, &mut definitions, &mut next_id, &mut messages).await,
            Err(reason) => error(reason.to_string(), false),
        };
        let fatal = reply.fatal;
        send_reply(reply)?;
        if fatal {
            break;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn read_ignores_stale_values() {
        let (tx, mut rx) = mpsc::channel(3);
        tx.send(Message::Value(1, 4, 10.0)).await.unwrap();
        tx.send(Message::Value(2, 3, 20.0)).await.unwrap();
        tx.send(Message::Value(2, 4, 30.0)).await.unwrap();
        let reply = wait_for_reply(&mut rx, 4, 6, 9, Some(2), Duration::from_millis(20)).await;
        assert_eq!(reply.value, Some(30.0));
    }

    #[tokio::test]
    async fn write_reports_the_matching_exception() {
        let (tx, mut rx) = mpsc::channel(2);
        tx.send(Message::Exception(8, "unrelated".into()))
            .await
            .unwrap();
        tx.send(Message::Exception(9, "rejected".into()))
            .await
            .unwrap();
        let reply = wait_for_reply(&mut rx, 4, 6, 9, None, Duration::from_millis(20)).await;
        assert_eq!(
            reply.error.as_deref(),
            Some("SimConnect request failed: rejected")
        );
    }
}
