# Prometheus Universal Exporter — `modbus` Request Type Specification

This is the `modbus` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [MODBUS.md](MODBUS.md).

## `modbus`

A `modbus` collector reads registers and bits from a Modbus TCP device
(Modbus Application Protocol 1.1b3, Messaging on TCP/IP 1.0b) and hands
their values to the transforms as a JSON document. It MUST need nothing
outside Go's standard library, MUST be opt-in (`request_type_modbus`), and
MUST send no function but 1, 2, 3 and 4.

| Key | Default | Rule |
| --- | --- | --- |
| `unit` | `1` | 0 to 255. |
| `read` | — | **Required.** 1 to 64 reads. |
| `read[].name` | — | **Required.** Letters, digits and `_`, not starting with a digit, unique. |
| `read[].table` | `holding` | `holding` (function 3), `input` (4), `coil` (1) or `discrete` (2). |
| `read[].address` | `0` | The protocol's address, from 0; the read MUST end within 65535. |
| `read[].type` | `u16` | `u16`, `i16`, `u32`, `i32`, `u64`, `i64`, `f32`, `f64` or `string`; MUST be left out, or `bool`, for a table of bits. |
| `read[].count` | `1` | How many values; of a `string`, its registers. A read MUST ask for at most 125 registers or 2000 bits. |
| `read[].order` | `abcd` | `abcd`, `badc`, `cdab` or `dcba`; not for a table of bits. |

Of the common part it takes `allowed_targets` and `denied_targets`; a
static target's request block takes `timeout`.

- A target MUST be `host`, `host:port` or `modbus://host:port`, port 502,
  and MAY end in `/<unit>`, 0 to 255, which replaces the collector's
  `unit`; anything else MUST be refused, a probe's with `400` before
  anything is sent.
- A scrape MUST make one connection, send each read as one request in the
  collector's order, each with a transaction identifier of its own, and
  close it.
- An answer MUST be taken only when its protocol identifier is 0 and its
  transaction identifier, unit and function are the request's, and it
  holds as many bytes as the registers or bits asked take. An exception
  MUST fail the scrape naming the read, the exception's code and its
  meaning; any other answer MUST fail it naming what differs. What such
  an answer held, and an exception code the protocol does not define, MUST
  NOT be told to a probe's caller (§ 19.4).
- A register's bytes MUST be read by `order`: `badc` and `dcba` swap the
  two bytes of each register, `cdab` and `dcba` reverse the registers of a
  value. A `string` MUST be two characters a register, its registers never
  reversed, ended at its first NUL, with blanks at its end removed. The first bit asked
  MUST be the lowest bit of the first byte.
- The answer MUST be a JSON document with each read's value under its
  name: a number, `true` or `false`, or text, and a list of them when
  `count` is more than 1. A float that is no number MUST be `null`. The
  decoder MUST be `json`.
- The deadline MUST bound the whole scrape, 10 seconds when none is set.
  The verbose `url` label MUST be `modbus://host:port/<unit>` and
  `http_method` `READ`.
