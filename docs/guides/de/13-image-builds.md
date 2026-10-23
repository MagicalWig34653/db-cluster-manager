---
title: Image-Builds und Registries (Entwurf)
---

# Image-Builds und Registries (Entwurf)

[Handbuch-Index](index.md) | Zurück: [Konfigurationsreferenz](12-configuration-reference.md)

**Status: nur [Fakes].** Das Backend existiert (Plan-Vorschau, Freigabe-Digest, Build-Protokolle, Registry-Client) und wurde nur gegen Fakes getestet. Es ist nicht im Manager verdrahtet, hat keine Oberfläche und hat nie ein echtes Image gebaut oder gepusht. Nicht darauf verlassen.

## Was vorhanden ist

- **Komposition:** nginx-Version, optional PHP-FPM-Version, PHP-Erweiterungen und `php.ini`-Werte aus Positivlisten, strukturierte nginx-Optionen, ein größenbegrenzter eigener nginx-Ausschnitt, zusätzliche Pakete, Plattformen. Das Basis-Image muss per Digest festgelegt sein.
- **Plan vor dem Build:** `POST /image-builds` ohne `approvalDigest` liefert Dockerfile, Kontextdateien, Schritte, Warnungen und eine Erklärung. Ein Build startet nur mit dem Digest dieses Plans.
- **Registries:** `POST /registries` registriert eine externe GHCR-, Docker-Hub- oder generische Registry. Das Passwort wird als Secret gespeichert und weder zurückgegeben noch protokolliert. Eine vom Manager betriebene `registry:2` ist nur eine Deploy-Spezifikation.
- **Aufzeichnung:** Jeder Job speichert Spec-Hash, Digests der Basis-Images, Image-Digest, einen SBOM-Platzhalter (nicht erzeugt) und ein geschwärztes Protokoll.

## Grenzen

Kein echter Build, Push, keine SBOM, kein Schwachstellenscan, keine Signatur. Alpine-Paketversionen sind nicht festgelegt. Paketnamen und Versionskatalog sind ungeprüft. Die Ausschnittsprüfung ist eine Sperrliste, kein Parser. Siehe [Bekannte Grenzen](10-known-limits.md) und [STATUS.md](../../STATUS.md).
