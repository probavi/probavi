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
[![Aerospike](https://img.shields.io/badge/Aerospike-4B5563)](adapters/aerospike/README.md)
[![Apache Cassandra](https://img.shields.io/badge/Apache%20Cassandra-1287B1?logo=apachecassandra&logoColor=white)](adapters/cassandra/README.md)
[![Apache Solr](https://img.shields.io/badge/Apache%20Solr-D9411E?logo=apachesolr&logoColor=white)](adapters/solr/README.md)
[![ClickHouse](https://img.shields.io/badge/ClickHouse-FFCC01?logo=clickhouse&logoColor=333333)](adapters/clickhouse/README.md)
[![CouchDB](https://img.shields.io/badge/CouchDB-E42528?logo=apachecouchdb&logoColor=white)](adapters/couchdb/README.md)
[![DuckDB](https://img.shields.io/badge/DuckDB-FFF000?logo=duckdb&logoColor=333333)](adapters/duckdb/README.md)
[![Elasticsearch](https://img.shields.io/badge/Elasticsearch-005571?logo=elasticsearch&logoColor=white)](adapters/elasticsearch/README.md)
[![etcd](https://img.shields.io/badge/etcd-419EDA?logo=etcd&logoColor=white)](adapters/etcd/README.md)
[![Firebird](https://img.shields.io/badge/Firebird-4B5563)](adapters/firebird/README.md)
[![H2](https://img.shields.io/badge/H2-09476B?logo=h2database&logoColor=white)](adapters/h2/README.md)
[![InfluxDB](https://img.shields.io/badge/InfluxDB-22ADF6?logo=influxdb&logoColor=white)](adapters/influxdb/README.md)
[![MariaDB](https://img.shields.io/badge/MariaDB-003545?logo=mariadb&logoColor=white)](adapters/mariadb/README.md)
[![MongoDB](https://img.shields.io/badge/MongoDB-47A248?logo=mongodb&logoColor=white)](adapters/mongodb/README.md)
[![MySQL](https://img.shields.io/badge/MySQL-4479A1?logo=mysql&logoColor=white)](adapters/mysql/README.md)
[![Neo4j](https://img.shields.io/badge/Neo4j-4581C3?logo=neo4j&logoColor=white)](adapters/neo4j/README.md)
[![OpenSearch](https://img.shields.io/badge/OpenSearch-005EB8?logo=opensearch&logoColor=white)](adapters/opensearch/README.md)
[![Oracle Database](https://img.shields.io/badge/Oracle%20Database-4B5563)](adapters/oracle/README.md)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?logo=postgresql&logoColor=white)](adapters/postgres/README.md)
[![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?logo=prometheus&logoColor=white)](adapters/prometheus/README.md)
[![Qdrant](https://img.shields.io/badge/Qdrant-DC244C?logo=qdrant&logoColor=white)](adapters/qdrant/README.md)
[![QuestDB](https://img.shields.io/badge/QuestDB-4B5563)](adapters/questdb/README.md)
[![Redis](https://img.shields.io/badge/Redis-FF4438?logo=redis&logoColor=white)](adapters/redis/README.md)
[![SQL Server](https://img.shields.io/badge/SQL%20Server-4B5563)](adapters/mssql/README.md)
[![SQLite](https://img.shields.io/badge/SQLite-003B57?logo=sqlite&logoColor=white)](adapters/sqlite/README.md)
[![TDengine](https://img.shields.io/badge/TDengine-4B5563)](adapters/tdengine/README.md)
[![Valkey](https://img.shields.io/badge/Valkey-4B5563)](adapters/valkey/README.md)
[![VictoriaMetrics](https://img.shields.io/badge/VictoriaMetrics-621773?logo=victoriametrics&logoColor=white)](adapters/victoriametrics/README.md)
[![Weaviate](https://img.shields.io/badge/Weaviate-4B5563)](adapters/weaviate/README.md)
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
