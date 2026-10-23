---
title: Image-Builds und Registries (Entwurf)
---

# Image-Builds und Registries (Entwurf)

[Handbuch-Index](index.md) | Zurück: [Konfigurationsreferenz](12-configuration-reference.md)

**Status: nur [Fakes].** Das Backend existiert (Plan-Vorschau, Freigabe-Digest, Build-Protokolle, Registry-Client) und wurde nur gegen Fakes getestet. Es ist nicht im Manager verdrahtet, hat eine Oberfläche, die nur gegen den Mock-Server lief, und hat nie ein echtes Image gebaut oder gepusht. Nicht darauf verlassen.

## Was vorhanden ist

- **Komposition:** nginx-Version, optional PHP-FPM-Version, PHP-Erweiterungen und `php.ini`-Werte aus Positivlisten, strukturierte nginx-Optionen, ein größenbegrenzter eigener nginx-Ausschnitt, zusätzliche Pakete, Plattformen. Das Basis-Image muss per Digest festgelegt sein.
- **Plan vor dem Build:** `POST /image-builds` ohne `approvalDigest` liefert Dockerfile, Kontextdateien, Schritte, Warnungen und eine Erklärung. Ein Build startet nur mit dem Digest dieses Plans.
- **Registries:** `POST /registries` registriert eine externe GHCR-, Docker-Hub- oder generische Registry. Das Passwort wird als Secret gespeichert und weder zurückgegeben noch protokolliert. Eine vom Manager betriebene `registry:2` ist nur eine Deploy-Spezifikation.
- **Aufzeichnung:** Jeder Job speichert Spec-Hash, Digests der Basis-Images, Image-Digest, einen SBOM-Platzhalter (nicht erzeugt) und ein geschwärztes Protokoll.

## Grenzen

Kein echter Build, Push, keine SBOM, kein Schwachstellenscan, keine Signatur. Alpine-Paketversionen sind nicht festgelegt. Paketnamen und Versionskatalog sind ungeprüft. Die Ausschnittsprüfung ist eine Sperrliste, kein Parser. Siehe [Bekannte Grenzen](10-known-limits.md) und [STATUS.md](../../STATUS.md).

## In der Web-Oberfläche (nur Mock)

Die Seite **Image-Builds** listet aufgezeichnete Builds (Status, Ziel-Image, Image-Digest, SBOM-Platzhalter) und enthält den Webserver-Composer: nginx- und PHP-Version, PHP-Erweiterungen (nur die, die der Katalog für die gewählte PHP-Version anbietet), `php.ini`-Werte, nginx-Optionen, ein optionales eigenes Snippet, zusätzliche Pakete, Plattformen und das Push-Ziel. **Dockerfile und Plan anzeigen** zeigt das erzeugte Dockerfile, die weiteren Kontextdateien, Schritte und Warnungen sowie den Plan-Digest. Der Build-Knopf bleibt gesperrt, bis man bestätigt, genau diesen Plan geprüft zu haben; jede spätere Änderung am Formular macht den Plan ungültig. Das Basis-Image muss per Digest angegeben werden.

![Image-Composer mit erzeugtem Dockerfile und Plan](../../assets/screenshots/de-light-images.png)

*Bildschirmfoto aus dem Entwicklungs-Mock-Server (`npm run dev:mock`, erfundene Beispieldaten), nicht von einem echten Manager.*

Die Seite **Registries** listet Registries (nie Zugangsdaten, nur ob welche gespeichert sind) und fügt eine externe GHCR-, Docker-Hub- oder generische OCI-Registry hinzu. Das Passwortfeld ist nur schreibbar: Es wird einmal gesendet und sofort aus der Seite entfernt.

![Seite Registries mit Formular zum Hinzufügen](../../assets/screenshots/de-light-registries.png)

*Bildschirmfoto aus dem Entwicklungs-Mock-Server (`npm run dev:mock`, erfundene Beispieldaten), nicht von einem echten Manager.*

Beide Seiten liefen nur gegen den Mock-Server [Nur Mock].
