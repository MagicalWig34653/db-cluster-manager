---
title: Manager-HTTPS und ACME
---

# Manager-HTTPS und ACME

[Übersicht](index.md) · [Konfigurationsreferenz](12-configuration-reference.md)

Der Manager kann HTTPS selbst mit ACME HTTP-01 oder TLS-ALPN-01 bereitstellen [Fakes]. Standard ist Let's Encrypt **Staging**; dessen Zertifikaten vertrauen Browser nicht. Die Tests dieser Implementierung kontaktierten keine öffentliche CA und keine echte Infrastruktur. Dies betrifft nur API und Oberfläche; Agenten, Metastore-Peers und Datenbanken haben eigene TLS-Einstellungen.

Ergänzen Sie in Ihrer bestehenden `manager`-Konfiguration folgenden Abschnitt; `dataDir`, `bind`, Authentifizierung und weitere Einstellungen bleiben erforderlich:

```json
{
  "tls": {
    "acme": {
      "directoryUrl": "https://acme-staging-v02.api.letsencrypt.org/directory",
      "email": "ops@example.com",
      "domains": ["manager.example.com"],
      "challenge": "tls-alpn-01"
    }
  }
}
```

Start: `manager -config manager.json -listen 0.0.0.0:8443`. Öffentliches DNS muss jeden angegebenen Namen zu diesen Managern führen. Bei TLS-ALPN-01 muss öffentlicher TCP-Port **443** den TLS-Listener mit unverändertem SNI und `acme-tls/1` erreichen. Für HTTP-01 setzen Sie `challenge` auf `http-01` und optional `httpListen` (Standard `:80`); öffentlicher TCP-Port **80** muss diesen separaten Listener erreichen. Er liefert ausschließlich aktive Challenge-Antworten, keine API, Oberfläche oder Weiterleitung. Portweiterleitung und Berechtigungen richten Sie selbst ein. Beim mitgelieferten Container ohne Root und Capabilities ist das Binden privilegierter Ports nicht vorauszusetzen; nutzen Sie lokale hohe Ports und externe Weiterleitung. Die ausgelieferten Deployment-Dateien verwenden weiterhin Loopback-HTTP hinter Ihrem Proxy und werden nicht automatisch umgestellt.

Ein Proxy, der TLS und ALPN selbst terminiert, verhindert TLS-ALPN-01. Nutzen Sie TCP-Durchleitung oder HTTP-01 mit Weiterleitung des Challenge-Pfads. Alle Manager-Replikate brauchen identische ACME-Einstellungen, denselben replizierten Metastore und Zugriff auf die Datenschlüssel (`manager.keyring` oder manuell derselbe Schlüssel). Zertifikate, Kontoschlüssel und kurzlebige Challenges werden vor Raft-Log und Snapshot mit AES-GCM verschlüsselt und an Collection/Schlüssel gebunden. Folgereplikate können aus ihrem lokalen replizierten Stand Zertifikate und Challenges liefern; Replikationsverzug oder nicht verfügbare Replikate können eine Challenge vorübergehend scheitern lassen. Die API muss weiterhin zum aktuellen Leader geroutet werden.

Nur der Leader bestellt und erneuert Zertifikate. Er prüft nach der Wahl und stündlich; ab 30 Tagen Restlaufzeit erneuert er, bei Fehlern versucht er es nach fünf Minuten erneut. Führungsverlust bricht die Bestellung ab und sperrt Schreibzugriffe sowie Challenge-Bereinigung des alten Terms. Ein neuer Leader verwendet den gespeicherten Kontoschlüssel und darf eine neue Bestellung beginnen. Fehler bei der Erneuerung erhalten das bisherige Zertifikat. Ohne erstes gültiges Zertifikat schlagen HTTPS-Handshakes bis zur erfolgreichen Ausstellung fehl. Das Manager-Log meldet `ACME issuance/renewal failed`. Es gibt keinen HTTP-Rückfall und keine Bestellung durch Client-Handshakes.

ACME-Aktivierung akzeptiert die Bedingungen der gewählten CA. Verwenden Sie eine reine E-Mail-Adresse und kleingeschriebene ASCII-/Punycode-DNS-Namen. IP-Adressen und Wildcards werden abgelehnt; DNS-01 bleibt eine separate, nicht angebundene Schnittstelle. External Account Binding, Widerruf und Kontoschlüsselwechsel sind nicht implementiert. Konfigurationsänderungen erfordern einen Neustart. Andere CA, E-Mail, Domains oder Challenge wählen einen eigenen gespeicherten Zustand; alte Datensätze bleiben verschlüsselt im Metastore.

Nach erfolgreichem Staging-Test stellen Sie `directoryUrl` ausdrücklich auf `https://acme-v02.api.letsencrypt.org/directory` um. Das bestellt neue Produktionszertifikate; Staging-Zertifikate werden nicht übernommen. `-healthcheck -config manager.json -listen 0.0.0.0:8443` nutzt geprüftes HTTPS über Loopback mit der ersten Domain als SNI. Bei Staging oder privater CA muss deren Zertifikat im Vertrauensspeicher liegen, beispielsweise über `SSL_CERT_FILE`. Die Prüfung wird nie abgeschaltet. Die Container-Startfrist muss für die erste Ausstellung ausreichen.

Für importierte Zertifikate setzen Sie stattdessen `manager.tls.certFile` und `keyFile`. Beide PEM-Dateien sind nötig und werden beim Start geladen; Austausch braucht einen Neustart. Reines HTTP ist ohne zusätzliche Freigabe nur auf numerischen Loopback-Adressen erlaubt. Für einen exponierten HTTP-Listener ist `manager.tls.allowPlainHttp: true` erforderlich, gedacht für Ihren TLS-Proxy in einem geschützten Netz. Hostnamen einschließlich `localhost` werden ohne ausdrückliche Konfiguration abgelehnt; verwenden Sie `127.0.0.1` oder `::1`. Aktivieren Sie die HTTP-Freigabe nicht für einen direkt öffentlichen Listener.

Geprüft werden eine falsche RFC-8555-CA mit `httptest`-Response-Recordern, beide Challenge-Nachweise, Erneuerung, Fehler und Abbruch, ein TLS-Handshake über `net.Pipe` sowie Speicherung und Leaderwechsel mit drei In-Memory-Raft-Knoten. Zusätzliche Tests mit einem Socket-gebundenen `httptest.Server` werden bei Sandbox-Sperren übersprungen; ein Skip belegt keinen funktionierenden Netzwerk-Listener. Siehe [aktuelle Ergebnisse](../../STATUS.md#manager-listener-acmetls-2026-10-24-fakes-only). Echtes DNS, öffentliche CA-Validierung, Browser-Vertrauen und ein HA-Loadbalancer bleiben ungeprüft.
