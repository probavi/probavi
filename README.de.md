<!-- i18n-source: README.md -->
<!-- i18n-span: intro sha256:0b3afd4c9bcca86fbeec4c2152c985e53748fc1a38706288b97fb0634a82921d -->
<!-- i18n-span: non-goals sha256:e100e9decc99337fb657e9e70709a723716108a104f3b03018a23724c597071d -->

# Probavi

[English](README.md) · [Magyar](README.hu.md) · **Deutsch** · [Français](README.fr.md) · [Español](README.es.md)

[![CI](https://github.com/probavi/probavi/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/probavi/probavi/actions/workflows/ci.yml)
[![CodeQL](https://github.com/probavi/probavi/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/probavi/probavi/actions/workflows/codeql.yml)
[![Coverage](https://codecov.io/gh/probavi/probavi/branch/main/graph/badge.svg)](https://codecov.io/gh/probavi/probavi)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/probavi/probavi/badge)](https://scorecard.dev/viewer/?uri=github.com/probavi/probavi)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/14080/badge)](https://www.bestpractices.dev/projects/14080)

[![Release](https://img.shields.io/github/v/release/probavi/probavi?sort=semver&label=release)](https://github.com/probavi/probavi/releases/latest)
[![License](https://img.shields.io/github/license/probavi/probavi?label=license)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/probavi/probavi?label=go)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS-informational)](docs/packaging.md)
[![Downloads](https://img.shields.io/github/downloads/probavi/probavi/total?label=downloads)](https://github.com/probavi/probavi/releases)

<!-- capabilities:engine-badges:start -->
[![Aerospike](https://img.shields.io/badge/Aerospike-informational)](adapters/aerospike/README.md)
[![Apache Cassandra](https://img.shields.io/badge/Apache%20Cassandra-informational)](adapters/cassandra/README.md)
[![Apache Solr](https://img.shields.io/badge/Apache%20Solr-informational)](adapters/solr/README.md)
[![ClickHouse](https://img.shields.io/badge/ClickHouse-informational)](adapters/clickhouse/README.md)
[![CouchDB](https://img.shields.io/badge/CouchDB-informational)](adapters/couchdb/README.md)
[![DuckDB](https://img.shields.io/badge/DuckDB-informational)](adapters/duckdb/README.md)
[![Elasticsearch](https://img.shields.io/badge/Elasticsearch-informational)](adapters/elasticsearch/README.md)
[![etcd](https://img.shields.io/badge/etcd-informational)](adapters/etcd/README.md)
[![Firebird](https://img.shields.io/badge/Firebird-informational)](adapters/firebird/README.md)
[![H2](https://img.shields.io/badge/H2-informational)](adapters/h2/README.md)
[![InfluxDB](https://img.shields.io/badge/InfluxDB-informational)](adapters/influxdb/README.md)
[![MariaDB](https://img.shields.io/badge/MariaDB-informational)](adapters/mariadb/README.md)
[![MongoDB](https://img.shields.io/badge/MongoDB-informational)](adapters/mongodb/README.md)
[![MySQL](https://img.shields.io/badge/MySQL-informational)](adapters/mysql/README.md)
[![Neo4j](https://img.shields.io/badge/Neo4j-informational)](adapters/neo4j/README.md)
[![OpenSearch](https://img.shields.io/badge/OpenSearch-informational)](adapters/opensearch/README.md)
[![Oracle Database](https://img.shields.io/badge/Oracle%20Database-informational)](adapters/oracle/README.md)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-informational)](adapters/postgres/README.md)
[![Prometheus](https://img.shields.io/badge/Prometheus-informational)](adapters/prometheus/README.md)
[![Qdrant](https://img.shields.io/badge/Qdrant-informational)](adapters/qdrant/README.md)
[![QuestDB](https://img.shields.io/badge/QuestDB-informational)](adapters/questdb/README.md)
[![Redis](https://img.shields.io/badge/Redis-informational)](adapters/redis/README.md)
[![SQL Server](https://img.shields.io/badge/SQL%20Server-informational)](adapters/mssql/README.md)
[![SQLite](https://img.shields.io/badge/SQLite-informational)](adapters/sqlite/README.md)
[![TDengine](https://img.shields.io/badge/TDengine-informational)](adapters/tdengine/README.md)
[![Valkey](https://img.shields.io/badge/Valkey-informational)](adapters/valkey/README.md)
[![VictoriaMetrics](https://img.shields.io/badge/VictoriaMetrics-informational)](adapters/victoriametrics/README.md)
[![Weaviate](https://img.shields.io/badge/Weaviate-informational)](adapters/weaviate/README.md)
<!-- capabilities:engine-badges:end -->

> **English is authoritative.** Dies ist eine Übersetzung der Einleitung von [README.md](README.md), Stand 2026-08-04. Bei Abweichungen gilt der englische Text: Installation, Beispiele und die aktuelle Aufstellung der Fähigkeiten sind nur auf Englisch aktuell.

*Probavi* — lateinisch für **„Ich habe bewiesen“.** Das Perfekt ist der Punkt: nicht „wir testen Restores“, sondern „dieser Restore wurde durchgeführt und bewiesen, hier ist der signierte Datensatz“.

**Sie haben Backups. Aber wann haben Sie zuletzt bewiesen, dass sie sich wiederherstellen lassen?**

Probavi ist eine selbst gehostete, engine-unabhängige Plattform für **kontinuierliche Restore-Verifikation**. Probavi erstellt keine Backups — das erledigen Ihre vorhandenen Werkzeuge (pg_dump, pgBackRest, wal-g, mysqldump, …) bereits gut. Die Aufgabe von Probavi ist es, fortlaufend zu *beweisen*, dass diese Backups tatsächlich wiederherstellbar sind.

1. Nach Zeitplan nimmt Probavi ein echtes Backup und führt einen **echten Restore** in eine verwerfbare, isolierte Sandbox aus (z. B. einen Docker-Container).
2. Auf der wiederhergestellten Datenbank laufen **Prüfungen** — von „ist sie gestartet?“ über Zeilenzahlen und Datenaktualität bis zu eigenen SQL-Assertions.
3. Das Ergebnis wird in einem **signierten Nachweisdatensatz** festgehalten, der jede nachträgliche Manipulation sichtbar macht: was wiederhergestellt wurde, wann, wie lange es dauerte, was geprüft wurde und wie das Ergebnis lautete.

Das Ergebnis ist kein grüner Haken. Es ist eine auditfähige, kryptografisch verifizierbare Historie der Wiederherstellbarkeit Ihrer Organisation — einschließlich gemessener Wiederherstellungszeiten (RTO) und ihres Verlaufs.

## Warum

- Die Logzeile „backup completed successfully“ beweist fast nichts. Backups scheitern lautlos: Datenkorruption, fehlende WAL-Segmente, Versionskonflikte, verlorene Verschlüsselungsschlüssel, monatelang die falschen Datenbanken gesichert.
- Vorschriften verlangen zunehmend eine *getestete und dokumentierte* Wiederherstellungsfähigkeit, nicht nur Backups (siehe die EU-Verordnung DORA, die NIS2-Richtlinie und die NIST-Leitlinien zur Notfallplanung).
- Cloud-Anbieter bieten Restore-Tests für ihre eigenen Managed Services an. Wenn Sie Datenbanken auf eigenen VMs, auf Bare Metal oder in einer gemischten Landschaft betreiben, gibt es kein neutrales, offenes Werkzeug, das dies für Sie tut. Probavi ist dieses Werkzeug.

## Nicht-Ziele

Probavi wird **keine** Backups erstellen, **keinen** eigenen Scheduler implementieren, **keine** Datenbank-Zugangsdaten über das hinaus verwalten, was ein Drill braucht, und **nicht** versuchen, eine Monitoring-Plattform zu sein. Kleiner Kern, scharf umrissener Zweck.

## Die CLI spricht auch Deutsch

`PROBAVI_LANG=de probavi run --config drill.yaml` — Hilfetext und Diagnosemeldungen erscheinen auf Deutsch. Maschinelle Ausgaben werden nie übersetzt: Nachweisdatensätze, JSON-Zusammenfassungen, das Adapterprotokoll und die Logs sind Verträge und bleiben immer auf Englisch ([docs/i18n.md](docs/i18n.md)).

## Weiter (auf Englisch)

- [README.md](README.md) — Status, Installation, Quickstart, Sandbox-Provider, Zeitplanung, Benachrichtigungen, DR-Game-Day
- [docs/](docs/) — normative Spezifikationen: Adapterprotokoll, Nachweisschema, i18n, Benachrichtigungen
- [docs/capabilities.json](docs/capabilities.json) — maschinenlesbare, generierte Aufstellung dessen, was Probavi heute kann
- [ROADMAP.md](ROADMAP.md) · [CHANGELOG.md](CHANGELOG.md) · [AGENTS.md](AGENTS.md) · [LICENSE](LICENSE) (Apache-2.0)
