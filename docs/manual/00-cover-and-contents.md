---
title: AirView
subtitle: User and Operator Manual
edition: Manual 1.0
software: AirView 1.0.1 and AirView Feeder 1.0.1
date: November 2026
covers: Windows station · Raspberry Pi station · Linux aggregator · Raspberry Pi, Linux and Windows feeders · MLAT · API reference
---

# About this manual

This manual describes AirView 1.0.1: the AirView **station** for Windows and the Raspberry Pi, the AirView
**aggregator** that runs an AirView Network on a Linux server, and **AirView Feeder** (`airview-feeder`) for the
Raspberry Pi, Linux and Windows. Everything in it was checked against release 1.0.0 and updated for 1.0.1. The screenshots come from
demo stations and a demonstration network, so the aircraft, stations and figures in them are simulated.

## Who should read what

| You want to | Read |
|---|---|
| Install AirView on a Windows PC or a Raspberry Pi next to your receiver | Chapter 1 (sections 1.1 to 1.3, 1.7 and 1.8), then chapter 2 |
| Learn the pages of a station: map, statistics, labs, alerts, reports, settings | Chapter 2 |
| Run your own AirView Network on a Linux server | Sections 1.4, 1.7 and 1.8, then chapter 3, especially 3.2 |
| Share your receiver's data with an AirView Network | Sections 1.5 or 1.6, then 3.3 |
| Understand how AirView locates aircraft by multilateration (MLAT) | Section 3.4 |
| Look up a setting, a command, an API endpoint, a port or a term | Chapter 4 |

## How the manual is organised

1. **Introduction and installation**: what AirView is made of, and how to install, set up and update every
   part of it.
2. **The station interface**: every page of a single station, in the order of the sidebar.
3. **AirView Network**: how a network fits together, running an aggregator, feeding a network, and MLAT.
4. **Reference**: configuration, command line, HTTP API, troubleshooting, glossary, licences.

## Conventions

| Shown as | Means |
|---|---|
| **Settings → Web server** | A page, tab, section, button or field of the interface, with `→` leading from a page to a part of it. |
| `airview config check` | Something you type, a file name, a setting path or a value. |
| `<version>`, `<host>` | A placeholder: replace it, including the angle brackets, with your own value. |
| `adsb.example.org` | The host name of an example network. Use your own network's host name. |
| `AIRVIEW_HTTP_PORT` | An environment variable. Every setting of `airview` has one (chapter 4.1). |
| `sudo` | A command that needs administrator rights on Linux. On Windows, use an administrator prompt instead. |

Commands for Linux are written for a Debian-based system (Raspberry Pi OS, Debian, Ubuntu) with systemd.
Commands for Windows are written for PowerShell unless stated otherwise.

## Ports used by default

| Port | Used for |
|---|---|
| 80 (TCP) | Station web interface; falls back to 8080–8089 when 80 is taken. The aggregator listens on 127.0.0.1:8080 behind a reverse proxy. |
| 30154 (TCP) | Station data port (hub) for remote-data clients |
| 30160 (TCP) | Aggregator: AVF/1 feeders and station uplinks, TLS 1.3 |
| 30004 (TCP) | Aggregator: Beast feeds from readsb / Ultrafeeder |
| 31090 (TCP) | Aggregator: mlat-client |
| 8099 (TCP) | Feeder status page, on 127.0.0.1 by default |

The full list is in chapter 4, **Ports at a glance**.

## Important notes for release 1.0.1

- **Smaller database, fewer disk writes.** The first start of 1.0.1 converts an existing 1.0.0 database in the
  background (about 80 % smaller); the station keeps working meanwhile, and a backup is written first when the
  disk has room. Going back to 1.0.0 needs that backup. On a Raspberry Pi with the data on an SD card AirView
  uses the **low-wear** storage profile automatically. Details: section 4.1, "Storage profiles".
- **Receiver-neutral factory settings.** A new installation starts with one omnidirectional antenna and a Beast
  feed from `localhost:30005` (readsb or dump1090 on the same machine) instead of a Radarcape with two antennas.
  Existing configurations are not changed.
- The Windows installers are not code-signed. Windows SmartScreen warns the first time you run them; section 1.2
  explains how to continue.
- All MLAT accuracy figures in this manual come from simulations. Every network measures its own accuracy
  continuously (section 3.4).
- AirView is for information and hobby use. **It must not be used for navigation or safety purposes.**
- Receiving and sharing aeronautical radio signals is regulated differently from country to country. You are
  responsible for complying with the law where you operate a receiver.
