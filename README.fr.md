<!-- i18n-source: README.md -->
<!-- i18n-span: intro sha256:0b3afd4c9bcca86fbeec4c2152c985e53748fc1a38706288b97fb0634a82921d -->
<!-- i18n-span: non-goals sha256:e100e9decc99337fb657e9e70709a723716108a104f3b03018a23724c597071d -->

# Probavi

[English](README.md) · [Magyar](README.hu.md) · [Deutsch](README.de.md) · **Français** · [Español](README.es.md)

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

> **English is authoritative.** Ceci est une traduction de l'introduction de [README.md](README.md), à jour au 2026-08-04. En cas de divergence, le texte anglais fait foi : l'installation, les exemples et l'inventaire des capacités ne sont à jour qu'en anglais.

*Probavi* — du latin **« j'ai prouvé ».** Le parfait latin est tout l'enjeu : non pas « nous testons les restaurations », mais « cette restauration a été effectuée et prouvée, voici l'enregistrement signé ».

**Vous avez des sauvegardes. Mais quand avez-vous prouvé pour la dernière fois qu'elles se restaurent ?**

Probavi est une plateforme auto-hébergée et indépendante des moteurs, dédiée à la **vérification continue des restaurations**. Elle ne réalise pas de sauvegardes — vos outils existants (pg_dump, pgBackRest, wal-g, mysqldump, …) le font déjà très bien. Le rôle de Probavi est de *prouver* en continu que ces sauvegardes sont réellement restaurables.

1. À intervalles planifiés, elle récupère une sauvegarde réelle et effectue une **véritable restauration** dans une sandbox jetable et isolée (par ex. un conteneur Docker).
2. Elle exécute des **vérifications** sur la base restaurée — de « a-t-elle démarré ? » aux assertions SQL personnalisées, en passant par les comptages de lignes et la fraîcheur des données.
3. Elle consigne le résultat dans un **enregistrement de preuve signé, où toute altération ultérieure devient visible** : ce qui a été restauré, quand, en combien de temps, ce qui a été vérifié et quel en a été le résultat.

Le résultat n'est pas une coche verte. C'est un historique auditable et cryptographiquement vérifiable de la capacité de reprise de votre organisation — y compris les temps de restauration mesurés (RTO) et leur évolution.

## Pourquoi

- La ligne de log « backup completed successfully » ne prouve presque rien. Les sauvegardes échouent en silence : corruption, segments WAL manquants, incompatibilités de version, clés de chiffrement perdues, mauvaises bases sauvegardées pendant des mois.
- La réglementation exige de plus en plus une capacité de reprise *testée et documentée*, et pas seulement des sauvegardes (voir le règlement européen DORA, la directive NIS2 et les recommandations du NIST en matière de planification de continuité).
- Les fournisseurs cloud proposent des tests de restauration pour leurs propres services managés. Si vous exploitez des bases sur vos propres VM, sur du bare metal ou dans un parc hétérogène, aucun outil neutre et ouvert ne le fait pour vous. Probavi est cet outil.

## Non-objectifs

Probavi ne réalisera **pas** de sauvegardes, n'implémentera **pas** son propre planificateur, ne gérera **pas** d'identifiants de base de données au-delà de ce dont un exercice a besoin, et ne tentera **pas** d'être une plateforme de supervision. Un noyau réduit, un objectif précis.

## La CLI parle aussi français

`PROBAVI_LANG=fr probavi run --config drill.yaml` — l'aide et les diagnostics s'affichent en français. Les sorties machine ne changent jamais de langue : les enregistrements de preuve, les résumés JSON, le protocole d'adaptateur et les logs sont des contrats et restent en anglais partout ([docs/i18n.md](docs/i18n.md)).

## Pour aller plus loin (en anglais)

- [README.md](README.md) — état, installation, démarrage rapide, providers de sandbox, planification, notifications, game-day de reprise d'activité (PRA)
- [docs/](docs/) — spécifications normatives : protocole d'adaptateur, schéma de preuve, i18n, notifications
- [docs/capabilities.json](docs/capabilities.json) — inventaire généré et lisible par machine de ce que Probavi sait faire aujourd'hui
- [ROADMAP.md](ROADMAP.md) · [CHANGELOG.md](CHANGELOG.md) · [AGENTS.md](AGENTS.md) · [LICENSE](LICENSE) (Apache-2.0)
