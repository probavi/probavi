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
