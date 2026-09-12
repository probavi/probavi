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
