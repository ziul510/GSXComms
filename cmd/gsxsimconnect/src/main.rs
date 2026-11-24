// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

use serde::{Deserialize, Serialize};
use simconnect::{
    OpenError, SimConnect,
    proto::{
        codec::PacketReader,
        enums::{DataRequestFlags, DataSetFlags, DataType, Period, RecvId},
        recv::{parse_exception, parse_sim_object_data},
    },
    transport::{self, DEFAULT_PIPE_NAME, PREPAR3D_PIPE_NAMES, Transport},
};
use std::{
    collections::HashMap,
    io::{self, BufRead, Write},
    sync::Arc,
    time::Duration,
};
use tokio::{
    sync::mpsc,
    time::{Instant, timeout, timeout_at},
};

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
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    uncertain: bool,
    #[serde(skip)]
    write_rejected: bool,
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
    read_sent: u32,
    request_id: u32,
    write_sent: Option<u32>,
    wait: Duration,
) -> Reply {
    let deadline = Instant::now() + wait;
    let mut read_reply = None;
    loop {
        // Match late reads by request and definition IDs and exceptions by send ID.
        let reply = match timeout_at(deadline, messages.recv()).await {
            Ok(Some(Message::Value(id, def, value))) if id == request_id && def == definition => {
                if value.is_finite() {
                    Reply {
                        value: Some(value),
                        ..Reply::default()
                    }
                } else {
                    error("Simulator returned a non-finite value", false)
                }
            }
            Ok(Some(Message::Exception(id, reason))) if id == registered => {
                return error(format!("SimConnect definition failed: {reason}"), true);
            }
            Ok(Some(Message::Exception(id, reason))) if Some(id) == write_sent => {
                return Reply {
                    write_rejected: true,
                    ..error(format!("SimConnect write failed: {reason}"), false)
                };
            }
            Ok(Some(Message::Exception(id, reason))) if id == read_sent => {
                error(format!("SimConnect request failed: {reason}"), false)
            }
            Ok(Some(Message::Disconnected(reason))) => return error(reason, true),
            Ok(None) => return error("SimConnect reader exited", true),
            Err(_) => {
                return read_reply.unwrap_or_else(|| error("SimConnect read timed out", false));
            }
            _ => continue,
        };
        if write_sent.is_none() {
            return reply;
        }
        // Keep waiting for a write exception even if the value already matches.
        read_reply.get_or_insert(reply);
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
            // Keep a variable's definition and the registration send ID for this connection
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
    let (write, wait) = match request {
        Request::Get { timeout_ms, .. } => (None, Duration::from_millis(timeout_ms.clamp(1, 850))),
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
            let sent = match sim
                .set_data_on_sim_object(
                    definition,
                    0,
                    DataSetFlags::DEFAULT,
                    0,
                    8,
                    &value.to_le_bytes(),
                )
                .await
            {
                Ok(sent) => sent,
                Err(reason) => return error(reason.to_string(), true),
            };
            (Some((sent, value)), Duration::from_millis(350))
        }
    };
    // A correlated read checks the written state and gives the server time to report rejection.
    *next_id = next_id.wrapping_add(1);
    let read_sent = match sim
        .request_data_on_sim_object(
            *next_id,
            definition,
            0,
            Period::Once,
            DataRequestFlags::empty(),
            0,
            0,
            0,
        )
        .await
    {
        Ok(sent) => sent,
        Err(reason) => return error(reason.to_string(), true),
    };
    let reply = wait_for_reply(
        messages,
        definition,
        registered,
        read_sent,
        *next_id,
        write.map(|(sent, _)| sent),
        wait,
    )
    .await;
    let Some((_, value)) = write else {
        return reply;
    };
    if reply.fatal || reply.write_rejected {
        return reply;
    }
    if reply.value == Some(value) {
        return Reply::default();
    }
    // GSX can consume a selection before the readback. The caller must avoid replaying it.
    Reply {
        uncertain: true,
        ..Reply::default()
    }
}

async fn open_simconnect() -> Result<SimConnect, String> {
    const APP_NAME: &str = "GSXComms";
    match SimConnect::open_local(APP_NAME).await {
        Ok(sim) => return Ok(sim),
        Err(OpenError::Io(err)) if err.kind() == io::ErrorKind::NotFound => {}
        Err(err) => return Err(err.to_string()),
    }

    // An MSFS configuration can expose a different SimConnect pipe
    let mut failure =
        "No local SimConnect pipe is available. Start Microsoft Flight Simulator and try again."
            .to_owned();
    for pipe in transport::scan_for_pipe_names("SimConnect") {
        if pipe.eq_ignore_ascii_case(DEFAULT_PIPE_NAME)
            || PREPAR3D_PIPE_NAMES
                .iter()
                .any(|name| pipe.eq_ignore_ascii_case(name))
        {
            continue;
        }
        let open = SimConnect::open_with(APP_NAME, || async {
            transport::connect_pipe(&pipe)
                .await
                .map(|client| Box::new(client) as Box<dyn Transport>)
        });
        // A stale endpoint must leave time to try the next one
        match timeout(Duration::from_secs(1), open).await {
            Ok(Ok(sim)) => return Ok(sim),
            Ok(Err(OpenError::Io(err))) if err.kind() == io::ErrorKind::NotFound => {}
            Ok(Err(err)) => failure = format!("Could not open a local SimConnect pipe: {err}"),
            Err(_) => failure = "Local SimConnect handshake timed out.".to_owned(),
        }
    }
    Err(failure)
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> io::Result<()> {
    let sim = match open_simconnect().await {
        Ok(sim) => Arc::new(sim),
        Err(reason) => return send_reply(error(reason, true)),
    };
    send_reply(Reply::default())?;
    // Read the parent pipe on its own thread to keep SimConnect responsive
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
    // End the connection on EOF or a fatal reply
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
        let reply = wait_for_reply(&mut rx, 4, 6, 9, 2, None, Duration::from_millis(20)).await;
        assert_eq!(reply.value, Some(30.0));
    }

    #[tokio::test]
    async fn write_reports_the_matching_exception() {
        let (tx, mut rx) = mpsc::channel(2);
        tx.send(Message::Exception(8, "unrelated".into()))
            .await
            .unwrap();
        // An early readback must leave time to receive the matching write exception.
        tx.send(Message::Value(2, 4, 0.0)).await.unwrap();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(150)).await;
            tx.send(Message::Exception(9, "rejected".into()))
                .await
                .unwrap();
        });
        let reply = wait_for_reply(&mut rx, 4, 6, 10, 2, Some(9), Duration::from_millis(350)).await;
        assert_eq!(
            reply.error.as_deref(),
            Some("SimConnect write failed: rejected")
        );
    }
}
