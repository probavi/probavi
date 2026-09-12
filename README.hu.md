<!-- i18n-source: README.md -->
<!-- i18n-span: intro sha256:0b3afd4c9bcca86fbeec4c2152c985e53748fc1a38706288b97fb0634a82921d -->
<!-- i18n-span: non-goals sha256:e100e9decc99337fb657e9e70709a723716108a104f3b03018a23724c597071d -->

# Probavi

[English](README.md) · **Magyar** · [Deutsch](README.de.md) · [Français](README.fr.md) · [Español](README.es.md)

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

> **English is authoritative.** Ez a [README.md](README.md) bevezetőjének fordítása, a 2026-08-04-i állapot szerint. Eltérés esetén az angol szöveg az irányadó: a telepítés, a példák és az aktuális képességlista csak angolul naprakész.

*Probavi* — latinul **„bebizonyítottam”.** A befejezett múlt a lényeg: nem „teszteljük a visszaállításokat”, hanem „ez a visszaállítás megtörtént és bizonyított, itt az aláírt rekord”.

**Vannak mentéseid. De mikor bizonyítottad utoljára, hogy visszaállíthatók?**

A Probavi önállóan üzemeltethető, motorfüggetlen platform **a visszaállítások folyamatos igazolására**. Nem készít mentést — azt a meglévő eszközeid (pg_dump, pgBackRest, wal-g, mysqldump, …) már jól csinálják. A Probavi dolga az, hogy folyamatosan *bizonyítsa*: azok a mentések tényleg helyreállíthatók.

1. Ütemezetten fog egy valódi mentést, és **valódi visszaállítást** hajt végre egy eldobható, izolált sandboxba (például egy Docker-konténerbe).
2. **Ellenőrzéseket** futtat a visszaállított adatbázison — az „elindult-e?” kérdéstől a sorok számán és az adatfrissességen át az egyedi SQL-állításokig.
3. Az eredményt **aláírt bizonyítékrekordként rögzíti, amelyen minden utólagos módosítás láthatóvá válik**: mit állított vissza, mikor, mennyi ideig tartott, mit ellenőrzött, és mi lett az eredmény.

A kimenet nem egy zöld pipa. Auditálható, kriptográfiailag ellenőrizhető történet a szervezeted helyreállíthatóságáról — a mért visszaállítási időkkel (RTO) és azok időbeli alakulásával együtt.

## Miért

- A „backup completed successfully” naplósor szinte semmit nem bizonyít. A mentések csendben romlanak el: sérülés, hiányzó WAL-szegmensek, verzióeltérések, elveszett titkosítókulcsok, hónapokon át rossz adatbázisok mentése.
- A szabályozások egyre inkább *tesztelt és dokumentált* helyreállítási képességet várnak el, nem csupán mentéseket (lásd EU DORA, NIS2 és a NIST vészhelyzeti tervezési ajánlásai).
- A felhőszolgáltatók a saját menedzselt szolgáltatásaikhoz kínálnak visszaállítási tesztelést. Ha saját VM-eken, fizikai gépeken vagy vegyes környezetben futtatsz adatbázisokat, nincs semleges, nyílt eszköz, amelyik ezt megtenné helyetted. A Probavi ez az eszköz.

## Nem célok

A Probavi **nem** fog mentést készíteni, nem implementál saját ütemezőt, nem kezel adatbázis-hitelesítő adatokat azon túl, amire egy drillnek szüksége van, és nem próbál monitoringplatform lenni. Kis mag, éles fókusz.

## A CLI is beszél magyarul

`PROBAVI_LANG=hu probavi run --config drill.yaml` — a súgó és a hibaüzenetek magyarul jelennek meg. A gépi kimenetek soha nem fordulnak le: a bizonyítékrekordok, a JSON-összegzések, az adapterprotokoll és a naplók szerződések, és mindig angolul maradnak ([docs/i18n.md](docs/i18n.md)).

## Tovább (angolul)

- [README.md](README.md) — állapot, telepítés, quickstart, sandbox-szolgáltatók, ütemezés, értesítések, DR game-day
- [docs/](docs/) — normatív specifikációk: adapterprotokoll, bizonyítékséma, i18n, értesítések
- [docs/capabilities.json](docs/capabilities.json) — géppel olvasható, generált kimutatás arról, mit tud ma a Probavi
- [ROADMAP.md](ROADMAP.md) · [CHANGELOG.md](CHANGELOG.md) · [AGENTS.md](AGENTS.md) · [LICENSE](LICENSE) (Apache-2.0)
