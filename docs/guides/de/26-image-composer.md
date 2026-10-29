---
title: Image-Composer
---

# Image-Composer [Nur mit Fakes und Mock-Server geprüft; daraus wurde noch kein Image gebaut]

Mit dem Image-Composer ([ADR 0026](../../decisions/0026-image-composer.md), Anforderung X17) stellen Sie den Inhalt eines Service-Images per Auswahl zusammen, statt ein Dockerfile zu schreiben: Basis-Distribution, nginx mit Version und Einstellungen, PHP mit Version, Erweiterungen, `php.ini`- und PHP-FPM-Pool-Einstellungen, Composer und einige Hilfsprogramme. Der Manager prüft die Auswahl, erzeugt Dockerfile und Konfigurationsdateien, zeigt sie zur Durchsicht und baut erst, nachdem ein Administrator genau diesen Plan freigegeben hat, über den vorhandenen Image-Build-Dienst (die Seite **Image-Builds** listet die entstandenen Jobs).

**Stand.** Nur Fakes und der Entwicklungs-Mock-Server. Der Komponentenkatalog ist als `unverified` markiert: Paketnamen und Versionen wurden aus Wissen geschrieben und nicht gegen echte Paketquellen geprüft, und aus einem erzeugten Rezept wurde noch kein Image gebaut. Der echte Build ist die letzte Prüfung und kann einen Eintrag ablehnen. Der Manager verdrahtet die Composer-Endpunkte noch nicht (503, bis er mit einem Build-Dienst aufgebaut wird).

## Die Seite benutzen

Öffnen Sie **Image-Composer** in der Navigation.

1. **Basis-Image.** Vergeben Sie einen Namen, wählen Sie Distribution und Release (Alpine 3.20, Debian 12, Ubuntu 24.04 LTS, AlmaLinux 9) und tragen Sie den Digest des Basis-Images ein (`sha256:` plus 64 Hex-Zeichen). Ein schwebender Tag wird nie gebaut. Den Digest ermitteln Sie selbst, zum Beispiel mit `docker buildx imagetools inspect docker.io/library/alpine:3.20`.
2. **Komponenten.** Haken Sie die gewünschten Komponenten an. Die Versionsliste zeigt nur Versionen, die es auf dem gewählten Release gibt. Erweiterungen hängen von der Distributionsfamilie ab. Eine Komponente, die eine andere braucht (Composer braucht PHP), lässt sich nicht ohne sie wählen. Genau eine Komponente ist der Hauptprozess: nginx; PHP-FPM ist ein Begleitprozess neben nginx oder der einzige Hauptprozess, wenn nginx nicht gewählt ist.
3. **Einstellungen.** Jede Komponente zeigt ihre erlaubten Einstellungen mit Standardwert, zulässigem Bereich und gegebenenfalls einem Hinweis. Gesendet werden nur Einstellungen, die vom Standard abweichen. Einstellungen, die nicht greifen können, sind ausgeblendet (zum Beispiel `opcache.*` ohne die Opcache-Erweiterung oder das FastCGI-Timeout ohne PHP).
4. **Durchsicht.** Rechts folgt die Antwort des Managers jeder Änderung (etwa 300 ms nach der letzten Eingabe): Befunde mit Pfad, die Varianten je Distribution, das erzeugte Dockerfile und jede Konfigurationsdatei (Datei in der Liste wählen), die Paketliste, Grenzen vor dem Bauen sowie Hinweise zu Härtung und Reproduzierbarkeit. Der Rezept-Digest identifiziert genau dieses Rezept.
5. **Varianten.** Jedes Release steht als *Kompatibel* oder *Weicht ab* in der Liste, mit dem, was abweicht, und der neuesten dort verfügbaren Version. *Diese Basis verwenden* wechselt die Basis und setzt Versionen auf das, was das Release bietet; Erweiterungen oder Komponenten, die es dort nicht gibt, entfallen, und das sehen Sie sofort.
6. **Planen und bauen.** Wählen Sie Registry, Repository und Tag und klicken Sie auf *Build planen*. Der Plan zeigt Schritte, Warnungen und den Plan-Digest. Setzen Sie die Freigabe und klicken Sie auf *Image bauen*. Jede Änderung an Rezept, Ziel oder Plattformen macht den Plan ungültig. Planen und Bauen brauchen die Administratorrolle; Operatoren können Rezepte ansehen.

## Was der Generator garantiert

- **Gleiches Rezept, gleiche Ausgabe.** Dockerfile und Kontextdateien sind für dasselbe normalisierte Rezept, dieselbe Katalog- und Generatorversion byteidentisch (Golden-File-Tests). Das Image selbst ist nur identisch, solange Basis-Digest und Paketquellen der Distribution unverändert sind: Pakete sind auf die Version major.minor eingeschränkt, ihre Patchstände schwimmen.
- **Härtungsstandards.** Basis per Digest festgelegt, Nicht-Root-Benutzer (uid 10001), unprivilegierter Port 8080 (nginx) oder 9000 (nur FPM), keine Geheimnisse oder Build-Argumente, keine Download-Skripte, keine Fremd-Repositories, `server_tokens` aus, Sicherheits-Header an, `expose_php`, `display_errors` und `allow_url_fopen` standardmäßig aus, `allow_url_include` aus und `cgi.fix_pathinfo=0` immer. `nginx -t` und `php-fpm -t` laufen beim Build.
- **Keine beliebige Shell.** Einstellungen sind typisiert (Ja/Nein, ganze Zahl mit Bereich, Auswahl aus einer Liste oder Text mit verpflichtendem Muster). Unbekannte Schlüssel, falsche Typen, Zahlen außerhalb des Bereichs und Shell-Sonderzeichen in einem musterprüften Feld werden mit dem Feldpfad abgelehnt.
- **Ein Hauptprozess.** Im Image gibt es keinen Supervisor. Stirbt PHP-FPM, während nginx weiterläuft, antworten Anfragen mit 502; nutzen Sie den Health-Check des Dienstes. Ein reines PHP-FPM-Image lauscht ohne Authentifizierung auf 9000: Betreiben Sie es in einem isolierten Netz.

## API

| Route | Rolle | Zweck |
| --- | --- | --- |
| `GET /api/v1/image-composer/catalog` | Viewer | Distributionen, Komponenten, Matrix, Optionen, Einstellungsschema (Beschriftungen DE/EN) |
| `POST /api/v1/image-composer/preview` | Operator | Prüfen und erzeugen; ungültige Rezepte liefern `valid: false` und `issues` |
| `POST /api/v1/image-composer/builds` | Administrator | Ohne `approvalDigest`: Build-Plan (200); mit dem Digest des Plans: startet den Build (202). Geändertes Rezept oder Ziel ergibt 409 `approval_mismatch` |

Ein Rezept hat `name`, `base {distro, release, digest}`, optional `platforms` und `components [{id, version, features, settings}]`. Das vollständige Schema steht in `api/openapi.yaml`.

## Grenzen

Nur Fakes; der Katalog ist ungeprüft; kein echter Build, kein SBOM, kein Scan (das SBOM bleibt `not_generated`, die Paketliste ist seine vorgesehene Eingabe). Vier Distributionen, 16 PHP-Erweiterungen, kein PECL und keine Quell-Builds, keine eigenen nginx-Snippets, ein PHP-FPM-Pool, eine Site. Die Seite ist nur gegen den Mock-Server geprüft (Screenshots müssen die Mock-Kennzeichnung tragen).
