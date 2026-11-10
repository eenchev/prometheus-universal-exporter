# Modbus

The `modbus` request type reads holding and input registers, coils and
discrete inputs from a device that speaks Modbus TCP — a PLC, an inverter,
an energy meter, or a gateway with serial devices behind it — and hands
their values to the transforms as a JSON document. The collector says
where each value is and what it is, a 32-bit float in two registers or a
bit, and the type reads it as a number, so a metric rule is one `jq`
expression away.

It only reads: the functions that write a register or a coil are not in
the code.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with modbus](#building-with-modbus)).

## Building with modbus

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=modbus              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=modbus -t exporter:modbus .
go build -tags "$(sh tools/request-type-tags.sh '' '' modbus)" .
```

The tag is `request_type_modbus`. A configuration with an `modbus` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: inverter
    request:
      type: modbus
      unit: 1
      read:
        - name: ac_power_watts
          table: input
          address: 30
          type: f32
        - name: phase_volts
          table: input
          address: 0
          type: f32
          count: 3
        - name: energy_total
          address: 72            # the holding table
          type: u32
          order: cdab
        - name: grid_connected
          table: discrete
          address: 4
    transform:
      type: jq
    metrics:
      - name: inverter_ac_power_watts
        expression: .ac_power_watts
      - name: inverter_phase_volts
        items: .phase_volts | to_entries[]
        expression: .value
        labels:
          - name: phase
            expression: .key + 1
      - name: inverter_energy_watt_hours_total
        type: counter
        expression: .energy_total
      - name: inverter_grid_connected
        expression: .grid_connected
```

```text
/probe?collector=inverter&target=10.0.4.20
```

```json
{"ac_power_watts": 4211.5, "phase_volts": [230.1, 229.8, 231.0], "energy_total": 18230411, "grid_connected": true}
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `modbus`. |
| `unit` | `1` | The unit id, 0 to 255, which tells the devices behind a gateway apart. A target may name its own. |
| `read` | — | **Required.** Up to 64 [reads](#reads). |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values). The decoder is `json`; use
`jq`, `yq` or `python`. A scale, such as a register in tenths of a volt, is
arithmetic in the expression: `.volts_raw / 10`.

## Reads

Each read is one request, sent one after another on the scrape's
connection, and its value is in the document under its `name`.

| Key | Default | Notes |
| --- | --- | --- |
| `name` | — | **Required.** Letters, digits and `_`, unique among the collector's reads. |
| `table` | `holding` | `holding` (function 3), `input` (4), `coil` (1) or `discrete` (2). |
| `address` | `0` | The first address, as the protocol numbers it, from 0. |
| `type` | `u16` | Of a register table: `u16`, `i16`, `u32`, `i32`, `u64`, `i64`, `f32`, `f64` or `string`. Left out for `coil` and `discrete`, whose values are `true` and `false`. |
| `count` | `1` | How many values; more than 1 makes the value a list. Of a `string`, how many registers it takes, two characters each. |
| `order` | `abcd` | Where a value's bytes are in its registers: `abcd` big-endian, `cdab` the registers swapped, `badc` the bytes of each register swapped, `dcba` both. |

A read asks for at most 125 registers or 2000 bits, the protocol's limit
for one request; split a longer run into several reads.

**Addresses.** Manuals often number registers from 1 with the table's digit
in front: 40001 is the first holding register, 30001 the first input
register. The protocol's address is that number without the digit, less
one: 40001 is `table: holding`, `address: 0`, and 30073 is `table: input`,
`address: 72`. A manual that says "address 72 (0x48)" usually means the
protocol's address already.

**Byte order.** The protocol sends each register's high byte first and says
nothing of values wider than a register. Most devices send the high
register first (`abcd`); many meters and inverters send the low register
first (`cdab`). A value that reads as an absurdly large or tiny number is
usually the other order.

A `u64` or `i64` past 2^53 is written exactly in the document, and read by
`jq` as the nearest floating-point number, as every sample is.

## The target

The target is `host`, `host:port` or `modbus://host:port`, port 502. A
target written `modbus://host:port/<unit>` asks that unit in place of the
collector's, so one collector reads every device behind a gateway:
`modbus://gateway.plant:502/17`. Anything else after the port, a user and
another scheme are refused with `400` before anything is sent. Logs and
labels show `modbus://host:port/<unit>`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `modbus` collector is a device,
written as a probe's target is, its unit in the target when it is not the
collector's. Its `request` may set `timeout`.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 10 seconds when it has neither. One connection is made
for each scrape and closed after its last read; many devices take only a
few connections at once, so scrape a device from one exporter, and no more
often than it answers.

## Errors and self-metrics

A device's exception fails the scrape, naming the read:

```text
request.read[2] (energy_total) of modbus://10.0.4.20:502/1: the device answered with exception 2, illegal data address: the device has no such address, or the read runs past its last one
```

So does an answer that is not the request's: another transaction, unit or
function, or fewer bytes than the registers asked for take. With
`web.self_metrics.verbose`, a scrape is labelled with
`modbus://host:port/<unit>` and `READ`.

## Limitations

- Modbus TCP only. Modbus RTU and ASCII, on a serial line or framed over
  TCP without the MBAP header, are not spoken; put a gateway in between.
- No TLS (Modbus/TCP Security, port 802).
- Nothing is written, and no diagnostic or device identification function
  is sent.
- A float that is not a number is `null` in the document.

## Troubleshooting

- **Exception 2, illegal data address**: the address is off by one, or
  written with the table's digit in front; see [Addresses](#reads).
- **Exception 11, gateway target device failed to respond**: no device
  answers as this unit. Check `unit`, or the target's `/<unit>`.
- **Values that make no sense**: try `order: cdab`, then the signed type,
  then the other table: some devices keep the same value in both.
- **`the device closed the connection before it answered`**: the device
  is at its limit of connections, or takes requests of unit 0 or 255
  alone.
