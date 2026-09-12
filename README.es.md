<!-- i18n-source: README.md -->
<!-- i18n-span: intro sha256:0b3afd4c9bcca86fbeec4c2152c985e53748fc1a38706288b97fb0634a82921d -->
<!-- i18n-span: non-goals sha256:e100e9decc99337fb657e9e70709a723716108a104f3b03018a23724c597071d -->

# Probavi

[English](README.md) · [Magyar](README.hu.md) · [Deutsch](README.de.md) · [Français](README.fr.md) · **Español**

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

> **English is authoritative.** Esta es una traducción de la introducción de [README.md](README.md), actualizada a 2026-08-04. En caso de discrepancia, prevalece el texto en inglés: la instalación, los ejemplos y el inventario de capacidades solo están actualizados en inglés.

*Probavi* — del latín **«he probado».** El pretérito perfecto es la clave: no «ponemos a prueba las restauraciones», sino «esta restauración se realizó y quedó probada, aquí está el registro firmado».

**Tiene copias de seguridad. Pero ¿cuándo probó por última vez que se restauran?**

Probavi es una plataforma autoalojada e independiente del motor para la **verificación continua de restauraciones**. No hace copias de seguridad — sus herramientas actuales (pg_dump, pgBackRest, wal-g, mysqldump, …) ya lo hacen bien. La tarea de Probavi es *probar* continuamente que esas copias son realmente recuperables.

1. De forma programada toma una copia de seguridad real y ejecuta una **restauración real** en una sandbox desechable y aislada (por ejemplo, un contenedor Docker).
2. Ejecuta **comprobaciones** sobre la base de datos restaurada — desde «¿arrancó?», pasando por los recuentos de filas y la actualidad de los datos, hasta las aserciones SQL propias.
3. Registra el resultado como un **registro de evidencia firmado en el que cualquier manipulación posterior queda a la vista**: qué se restauró, cuándo, cuánto tardó, qué se comprobó y cuál fue el resultado.

El resultado no es una marca verde. Es un historial auditable y criptográficamente verificable de la capacidad de recuperación de su organización — incluidos los tiempos de restauración medidos (RTO) y su evolución.

## Por qué

- La línea de log «backup completed successfully» no prueba casi nada. Las copias fallan en silencio: corrupción, segmentos WAL ausentes, versiones incompatibles, claves de cifrado perdidas, meses copiando las bases de datos equivocadas.
- La normativa exige cada vez más una capacidad de recuperación *probada y documentada*, no solo copias de seguridad (véanse el Reglamento europeo DORA, la Directiva NIS2 y las guías del NIST sobre planificación de contingencias).
- Los proveedores de nube ofrecen pruebas de restauración para sus propios servicios gestionados. Si ejecuta bases de datos en sus propias VM, en bare metal o en un entorno mixto, no hay ninguna herramienta neutral y abierta que lo haga por usted. Probavi es esa herramienta.

## No objetivos

Probavi **no** hará copias de seguridad, **no** implementará su propio planificador, **no** gestionará credenciales de base de datos más allá de lo que necesita un simulacro y **no** intentará ser una plataforma de monitorización. Núcleo pequeño, objetivo preciso.

## La CLI también habla español

`PROBAVI_LANG=es probavi run --config drill.yaml` — la ayuda y los diagnósticos se muestran en español. Las salidas de máquina nunca cambian de idioma: los registros de evidencia, los resúmenes JSON, el protocolo de adaptadores y los logs son contratos y permanecen en inglés en todas partes ([docs/i18n.md](docs/i18n.md)).

## Más información (en inglés)

- [README.md](README.md) — estado, instalación, inicio rápido, proveedores de sandbox, programación, notificaciones, game-day de recuperación
- [docs/](docs/) — especificaciones normativas: protocolo de adaptadores, esquema de evidencia, i18n, notificaciones
- [docs/capabilities.json](docs/capabilities.json) — inventario generado y legible por máquina de lo que Probavi hace hoy
- [ROADMAP.md](ROADMAP.md) · [CHANGELOG.md](CHANGELOG.md) · [AGENTS.md](AGENTS.md) · [LICENSE](LICENSE) (Apache-2.0)
