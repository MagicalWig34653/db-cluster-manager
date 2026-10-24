# Kubernetes-Grundlage (W10)

Dies ist eine **experimentelle Grundlage, ausschließlich mit Fakes geprüft**.
Kubernetes ist noch kein unterstütztes Deployment-Ziel. Kein R9-Abnahmekriterium
ist erfüllt. Zeitpunkt und erste unterstützte Distribution bleiben offene
Owner-Entscheidungen. Galera/R1 behält Vorrang.
Siehe [ADR 0016](../../decisions/0016-kubernetes-groundwork.md).

Der ausgelieferte Manager aktiviert und verdrahtet diese Funktion nicht.
Bibliotheksnutzer müssen Renderer/KubeAPI ausdrücklich aktivieren. Ein
API-Testhost muss `api.Config.ExperimentalKubernetes` setzen und das
Registrierungsbackend bereitstellen; auch dessen `Enabled` muss gesetzt sein.
W10 enthält keinen Produktionsschalter, keine CLI, Provisionierung und
keinen Kubernetes-Deployment- oder Recovery-Ablauf.

## Registrierungsoberfläche

In der Navigation **Kubernetes-Registrierungen** öffnen. Die Seite zeigt Metadaten
und das Vorhandensein von Zugangsdaten. Zur Registrierung dienen ID, Anzeigename,
HTTPS-API-Origin (ohne abschließenden Schrägstrich), Namespace und entweder ein
Bearer-Token oder eine unverarbeitete kubeconfig. ID und Namespace sind
kleingeschriebene DNS-Namen mit maximal 63 Zeichen. Zugangsdaten sind nur
schreibbar, auf 32 KiB begrenzt und werden nach jedem Sendeversuch geleert.
Auch ein Wechsel der Zugangsdatenart leert das Feld. Der Browser speichert keine
Zugangsdaten dauerhaft. Erfolg bedeutet gespeicherte Metadaten, keine Prüfung
von Erreichbarkeit, Kompatibilität oder Deployment-Bereitschaft.

Der Entwicklungs-Mock liefert erfundene Registrierungen ohne Clusterkontakt.
Der ausgelieferte Manager antwortet weiterhin mit 503, da sein Backend nicht
verdrahtet ist. Lade-, Berechtigungs-, Deaktivierungs- und Anfragefehler bleiben
sichtbar. Es gibt keinen Deployment- oder Verbindungstest-Knopf. Browserdarstellung
und Integration mit einem echten Manager sind für diese Seite noch ungeprüft.

Der HTTP-Vertrag umfasst ausschließlich Registrierung:

- `POST /api/v1/kubernetes-clusters`: `id`, `name`, eine HTTPS-Origin als
  `endpoint`, `namespace` und genau eines von `token` oder undurchsichtigem
  `kubeconfig` (bis 32 KiB).
- `GET /api/v1/kubernetes-clusters`: Metadaten, Art und Vorhandensein des
  Zugangsmittels.

Beide benötigen eine vollständige Sitzung und Berechtigungen auf der globalen
Ressource `kubernetes_cluster`. Registrierung benötigt zusätzlich die übliche
Same-Origin-Anfrage und `create`; Auflisten benötigt `read`. Nur die
Administrator-Vorlage enthält diese Rechte. Deaktivierte oder unverdrahtete
Endpunkte antworten nach der Zugriffsprüfung mit 503. Der Speicheradapter
verschlüsselt Zugangsmittel über den bestehenden Datenschlüsselpfad. Antworten
enthalten keine Zugangsmittel. Registrierung prüft weder Erreichbarkeit noch
Zugangsdaten und führt keine Kubeconfig-Plugins aus. Tests verwenden erfundene
Werte; keine Produktionszugangsdaten eintragen.

`service/k8s.Renderer.Definition` erzeugt aus einer Service-Definition eine
deterministische Vorschau mit Digest und YAML-Export in JSON-Schreibweise.
Sichtbar sind Workloads, Services, lokale PV/PVC, ConfigMaps, Secret-Referenzen
und unterstützte Traefik-Routen. Es gibt keinen Endpunkt zur
Ausführungsfreigabe. Alle Blocker müssen sichtbar bleiben; ein Export erlaubt
keine Anwendung mit externen Werkzeugen.

Lokaler Speicher benötigt pro Volume ausdrücklich Kubernetes-Hostname,
absoluten Datenpfad und Kapazität. PVs behalten Daten und sind an Node und
Claim gebunden. Swarm-Node-IDs sind keine Kubernetes-Hostnamen.
Identitätsmarkerprüfung und sichere Rückkehr eines Nodes sind noch blockiert.
Galera-StatefulSets zeigen nur die geplante Form: Bootstrap,
`safe_to_bootstrap`, Quorum, Donor-Prüfung und geführte Recovery sind für
Kubernetes nicht implementiert. Traefik IngressRoute benötigt Controller und
CRD; nicht abgebildete TLS-, Middleware- und Backend-Semantik blockiert den Plan.

Es wurde kein echter Cluster verwendet. Tests nutzen Fake-Objekte, einen
Fake-HTTP-Transport mit `httptest` und den replizierten Metastore im Prozess.
Tatsächliche Prüfergebnisse und Sandbox-Grenzen stehen im
[Status](../../STATUS.md#w10-kubernetes-groundwork).
