<!-- i18n-source: README.md -->
<!-- i18n-span: intro sha256:0b3afd4c9bcca86fbeec4c2152c985e53748fc1a38706288b97fb0634a82921d -->
<!-- i18n-span: non-goals sha256:e100e9decc99337fb657e9e70709a723716108a104f3b03018a23724c597071d -->

# Probavi

[English](README.md) · **Magyar** · [Deutsch](README.de.md) · [Français](README.fr.md) · [Español](README.es.md)

[![CI](https://github.com/probavi/probavi/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/probavi/probavi/actions/workflows/ci.yml)
[![CodeQL](https://github.com/probavi/probavi/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/probavi/probavi/actions/workflows/codeql.yml)
[![Coverage](https://codecov.io/gh/probavi/probavi/branch/main/graph/badge.svg)](https://codecov.io/gh/probavi/probavi)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/probavi/probavi/badge)](https://scorecard.dev/viewer/?uri=github.com/probavi/probavi)

[![Release](https://img.shields.io/github/v/release/probavi/probavi?sort=semver&label=release)](https://github.com/probavi/probavi/releases/latest)
[![License](https://img.shields.io/github/license/probavi/probavi?label=license)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/probavi/probavi?label=go)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS-informational)](docs/packaging.md)
[![Downloads](https://img.shields.io/github/downloads/probavi/probavi/total?label=downloads)](https://github.com/probavi/probavi/releases)

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
