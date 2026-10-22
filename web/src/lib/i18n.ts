export const locales = ['en', 'de'] as const;
export type Locale = (typeof locales)[number];

const en = {
	appName: 'DB Cluster Manager',
	prototype: 'Prototype',
	skipToContent: 'Skip to content',
	language: 'Language',
	openNavigation: 'Open navigation',
	closeNavigation: 'Close navigation',
	mainNavigation: 'Main navigation',
	signedInAs: 'Signed in as {account}',
	signOut: 'Sign out',
	loading: 'Loading…',
	retry: 'Retry',
	back: 'Back',
	continue: 'Continue',
	cancel: 'Cancel',

	navOverview: 'Overview',
	navSwarms: 'Swarms',
	navClusters: 'Clusters',
	navHosts: 'Hosts',
	navDatabases: 'Databases',
	navBackups: 'Backups',
	navDashboards: 'Dashboards',
	navOperations: 'Operations',
	navEvents: 'Events',
	navSettings: 'Settings',

	notAvailableTitle: 'Not available yet',
	notAvailableBody:
		'This section is not implemented in this version of the manager. Nothing is shown here until real data is available.',

	stateHealthy: 'Healthy',
	stateDegraded: 'Degraded',
	stateUnavailable: 'Unavailable',
	stateUnknown: 'Unknown',
	opPending: 'Pending',
	opRunning: 'Running',
	opSucceeded: 'Succeeded',
	opFailed: 'Failed',
	sevInfo: 'Info',
	sevWarning: 'Warning',
	sevCritical: 'Critical',

	errUnauthenticated: 'Your session has ended. Please sign in again.',
	errForbidden: 'You do not have permission to view or change this.',
	errOrigin:
		'The manager rejected the request because of its origin. Open the manager through its configured address.',
	errEnrollmentRequired: 'Register a new passkey before continuing.',
	errNotFound: 'Not found, or you do not have access to it.',
	errBadRequest: 'The request was not accepted. Check the entered values.',
	errConflict: 'This action is no longer possible in the current state.',
	errRateLimited: 'Too many attempts. Please wait and try again.',
	errRateLimitedSeconds: 'Too many attempts. Try again in {seconds} seconds.',
	errUnavailable: 'The manager cannot provide this right now (service unavailable).',
	errServer: 'The manager reported an internal error.',
	errInvalidResponse: 'The manager sent a response this UI does not understand.',
	errNetwork: 'The manager could not be reached.',

	ceremonyUnsupported: 'This browser does not support passkeys.',
	ceremonyInsecure:
		'Passkeys need a secure connection (HTTPS or localhost) and a matching address. Open the manager through its configured address.',
	ceremonyCancelled: 'The passkey prompt was cancelled or timed out.',
	ceremonyExists: 'This authenticator already holds a passkey for this account.',
	ceremonyFailed: 'The passkey could not be used.',

	observed: 'Observed',
	observedAgo: 'observed {age}',
	freshnessUnknown: 'freshness unknown',
	never: 'never',
	notReported: 'Not reported',
	notReportedHint: 'The manager API does not report this value yet; it is shown as unknown.',
	nothingYet: 'No entries.',

	overviewTitle: 'Overview',
	overviewIntro: 'Manager process, your session and the inventory the manager reports.',
	processStatus: 'Manager process',
	apiHealth: 'API health',
	healthOk: 'Serving requests',
	healthOnlyProcess: 'Reports only that the manager process serves requests, not cluster health.',
	version: 'Version',
	commit: 'Commit',
	go: 'Go',
	platform: 'Platform',
	inventory: 'Inventory',
	countSwarms: 'Swarms',
	countClusters: 'Clusters',
	byState: 'By state',
	authDisabled:
		'Authentication is not configured on this manager. Only process status is available; resource data requires an auth configuration.',

	swarmsTitle: 'Swarms',
	swarmsIntro: 'Docker Swarms connected to this manager.',
	swarmsEmpty: 'No Swarm is visible to you.',
	colName: 'Name',
	colId: 'ID',
	colState: 'State',
	colFreshness: 'Freshness',
	colSwarm: 'Swarm',
	colEngine: 'Engine',
	colNodes: 'Nodes',
	colHost: 'Host',
	colRole: 'Role',
	colKind: 'Kind',
	colTime: 'Time',
	colTarget: 'Target',
	colMessage: 'Message',

	clustersTitle: 'Clusters',
	clustersIntro: 'Database clusters you may read.',
	clustersEmpty: 'No cluster is visible to you.',
	clusterDetail: 'Cluster',
	endpoint: 'Endpoint',
	lastVerifiedRestore: 'Last verified restore',
	automationPolicy: 'Automation policy',
	nodes: 'Nodes',
	noNodes: 'No nodes reported.',
	backToClusters: 'Back to clusters',
	nodeCount: '{count} nodes',

	operationsTitle: 'Operations',
	operationsIntro: 'Operations the manager ran or is running, newest first.',
	operationsEmpty: 'No operations are visible to you.',
	started: 'Started',
	ended: 'Ended',
	startUnknown: 'start time unknown',

	eventsTitle: 'Events',
	eventsIntro: 'Events reported by the manager, newest first.',
	eventsEmpty: 'No events are visible to you.',
	managerLevel: 'Manager',

	settingsTitle: 'Settings',
	settingsIntro: 'Appearance and language apply to this browser. Your account and passkeys are managed below.',
	appearance: 'Appearance',
	theme: 'Theme',
	themeSystem: 'System',
	themeLight: 'Light',
	themeDark: 'Dark',
	accent: 'Accent color',
	accentZinc: 'Neutral',
	accentBlue: 'Blue',
	accentGreen: 'Green',
	accentViolet: 'Violet',
	accentOrange: 'Orange',
	preferencesLocal: 'Stored in this browser only; the manager does not store user preferences yet.',
	account: 'Account',
	accountId: 'Account',
	sessionSince: 'Signed in since',
	addPasskey: 'Add passkey',
	addPasskeyHint:
		'Registering a passkey ends all sessions of your account. This browser receives a new session automatically.',
	passkeyLabel: 'Passkey name (optional)',
	passkeyAdded: 'Passkey "{label}" registered.',
	passkeyAddedUnnamed: 'Passkey registered.',
	managerSettings: 'Manager settings',

	loginTitle: 'Sign in',
	loginIntro: 'Sign in with a passkey saved for this manager.',
	loginButton: 'Sign in with passkey',
	loginFailed: 'Sign-in failed. Use a passkey registered for this manager.',
	lostPasskey: 'Lost your passkey? Use account recovery.',

	setupTitle: 'Create the first administrator',
	setupIntro:
		'Enter the one-time setup token printed by the manager at start, choose an account name and register a passkey.',
	setupUnavailable:
		'Setup is no longer available: an administrator exists, or the setup token has expired. Sign in instead.',
	setupToken: 'Setup token',
	setupTokenHint: 'Printed once to the manager log when it starts.',
	accountName: 'Account name',
	accountNameHint: 'Letters, digits and . _ @ - (up to 64 characters).',
	displayName: 'Display name (optional)',
	registerPasskey: 'Register passkey and create administrator',
	setupWrongToken: 'The setup token was not accepted.',
	recoveryCodesTitle: 'Recovery codes',
	recoveryCodesIntro:
		'Store these codes somewhere safe now. Each code can be used once to regain access if you lose your passkeys. They are shown only this once.',
	recoveryCodesMissing:
		'Recovery codes could not be generated. Your administrator account works, but has no recovery codes yet.',
	copyCodes: 'Copy codes',
	downloadCodes: 'Download as text file',
	copied: 'Copied.',
	codesStored: 'I have stored these recovery codes safely.',
	continueToManager: 'Continue to the manager',

	recoverTitle: 'Account recovery',
	recoverIntro:
		'Redeem a recovery code. Your existing passkeys are removed and all sessions end; you then register a new passkey.',
	recoveryCode: 'Recovery code',
	redeemCode: 'Redeem recovery code',
	recoveryFailed: 'Recovery failed. Check the account name and code.',
	recoverTokenTitle: 'Recovery link',
	recoverTokenIntro:
		'This single-use link was issued through the manager CLI. Continuing removes the passkeys of account {account}, ends its sessions and lets you register a new passkey.',
	recoverTokenMissing: 'This recovery link is incomplete. Request a new one through the manager CLI.',
	redeemToken: 'Continue recovery',
	enrollTitle: 'Register a new passkey',
	enrollIntro: 'Recovery succeeded. Register a new passkey for account {account} to finish.',
	enrollButton: 'Register passkey',
	backToLogin: 'Back to sign in'
} as const;

export type MessageKey = keyof typeof en;

const de: Record<MessageKey, string> = {
	appName: 'DB Cluster Manager',
	prototype: 'Prototyp',
	skipToContent: 'Zum Inhalt springen',
	language: 'Sprache',
	openNavigation: 'Navigation öffnen',
	closeNavigation: 'Navigation schließen',
	mainNavigation: 'Hauptnavigation',
	signedInAs: 'Angemeldet als {account}',
	signOut: 'Abmelden',
	loading: 'Wird geladen …',
	retry: 'Erneut versuchen',
	back: 'Zurück',
	continue: 'Weiter',
	cancel: 'Abbrechen',

	navOverview: 'Übersicht',
	navSwarms: 'Swarms',
	navClusters: 'Cluster',
	navHosts: 'Hosts',
	navDatabases: 'Datenbanken',
	navBackups: 'Backups',
	navDashboards: 'Dashboards',
	navOperations: 'Vorgänge',
	navEvents: 'Ereignisse',
	navSettings: 'Einstellungen',

	notAvailableTitle: 'Noch nicht verfügbar',
	notAvailableBody:
		'Dieser Bereich ist in dieser Manager-Version noch nicht umgesetzt. Hier wird nichts angezeigt, bis echte Daten vorliegen.',

	stateHealthy: 'Gesund',
	stateDegraded: 'Beeinträchtigt',
	stateUnavailable: 'Nicht verfügbar',
	stateUnknown: 'Unbekannt',
	opPending: 'Ausstehend',
	opRunning: 'Läuft',
	opSucceeded: 'Erfolgreich',
	opFailed: 'Fehlgeschlagen',
	sevInfo: 'Info',
	sevWarning: 'Warnung',
	sevCritical: 'Kritisch',

	errUnauthenticated: 'Deine Sitzung ist beendet. Bitte melde dich erneut an.',
	errForbidden: 'Dafür fehlt dir die Berechtigung.',
	errOrigin:
		'Der Manager hat die Anfrage wegen ihrer Herkunft abgelehnt. Öffne den Manager über seine konfigurierte Adresse.',
	errEnrollmentRequired: 'Registriere zuerst einen neuen Passkey.',
	errNotFound: 'Nicht gefunden oder kein Zugriff.',
	errBadRequest: 'Die Anfrage wurde nicht angenommen. Prüfe die Eingaben.',
	errConflict: 'Diese Aktion ist im aktuellen Zustand nicht mehr möglich.',
	errRateLimited: 'Zu viele Versuche. Bitte warte und versuche es erneut.',
	errRateLimitedSeconds: 'Zu viele Versuche. Versuche es in {seconds} Sekunden erneut.',
	errUnavailable: 'Der Manager kann das gerade nicht bereitstellen (Dienst nicht verfügbar).',
	errServer: 'Der Manager hat einen internen Fehler gemeldet.',
	errInvalidResponse: 'Der Manager hat eine Antwort gesendet, die diese Oberfläche nicht versteht.',
	errNetwork: 'Der Manager ist nicht erreichbar.',

	ceremonyUnsupported: 'Dieser Browser unterstützt keine Passkeys.',
	ceremonyInsecure:
		'Passkeys benötigen eine sichere Verbindung (HTTPS oder localhost) und eine passende Adresse. Öffne den Manager über seine konfigurierte Adresse.',
	ceremonyCancelled: 'Die Passkey-Abfrage wurde abgebrochen oder ist abgelaufen.',
	ceremonyExists: 'Dieser Authenticator enthält bereits einen Passkey für dieses Konto.',
	ceremonyFailed: 'Der Passkey konnte nicht verwendet werden.',

	observed: 'Beobachtet',
	observedAgo: 'beobachtet {age}',
	freshnessUnknown: 'Aktualität unbekannt',
	never: 'nie',
	notReported: 'Nicht gemeldet',
	notReportedHint: 'Die Manager-API meldet diesen Wert noch nicht; er wird als unbekannt angezeigt.',
	nothingYet: 'Keine Einträge.',

	overviewTitle: 'Übersicht',
	overviewIntro: 'Manager-Prozess, deine Sitzung und der vom Manager gemeldete Bestand.',
	processStatus: 'Manager-Prozess',
	apiHealth: 'API-Status',
	healthOk: 'Nimmt Anfragen an',
	healthOnlyProcess: 'Meldet nur, dass der Manager-Prozess Anfragen annimmt, nicht den Cluster-Zustand.',
	version: 'Version',
	commit: 'Commit',
	go: 'Go',
	platform: 'Plattform',
	inventory: 'Bestand',
	countSwarms: 'Swarms',
	countClusters: 'Cluster',
	byState: 'Nach Zustand',
	authDisabled:
		'Auf diesem Manager ist keine Authentifizierung konfiguriert. Verfügbar ist nur der Prozessstatus; Ressourcendaten erfordern eine Auth-Konfiguration.',

	swarmsTitle: 'Swarms',
	swarmsIntro: 'Mit diesem Manager verbundene Docker Swarms.',
	swarmsEmpty: 'Für dich ist kein Swarm sichtbar.',
	colName: 'Name',
	colId: 'ID',
	colState: 'Zustand',
	colFreshness: 'Aktualität',
	colSwarm: 'Swarm',
	colEngine: 'Engine',
	colNodes: 'Knoten',
	colHost: 'Host',
	colRole: 'Rolle',
	colKind: 'Art',
	colTime: 'Zeit',
	colTarget: 'Ziel',
	colMessage: 'Meldung',

	clustersTitle: 'Cluster',
	clustersIntro: 'Datenbank-Cluster, die du lesen darfst.',
	clustersEmpty: 'Für dich ist kein Cluster sichtbar.',
	clusterDetail: 'Cluster',
	endpoint: 'Endpunkt',
	lastVerifiedRestore: 'Letzte verifizierte Wiederherstellung',
	automationPolicy: 'Automatisierungsrichtlinie',
	nodes: 'Knoten',
	noNodes: 'Keine Knoten gemeldet.',
	backToClusters: 'Zurück zu den Clustern',
	nodeCount: '{count} Knoten',

	operationsTitle: 'Vorgänge',
	operationsIntro: 'Vom Manager ausgeführte oder laufende Vorgänge, neueste zuerst.',
	operationsEmpty: 'Für dich sind keine Vorgänge sichtbar.',
	started: 'Gestartet',
	ended: 'Beendet',
	startUnknown: 'Startzeit unbekannt',

	eventsTitle: 'Ereignisse',
	eventsIntro: 'Vom Manager gemeldete Ereignisse, neueste zuerst.',
	eventsEmpty: 'Für dich sind keine Ereignisse sichtbar.',
	managerLevel: 'Manager',

	settingsTitle: 'Einstellungen',
	settingsIntro: 'Darstellung und Sprache gelten für diesen Browser. Konto und Passkeys verwaltest du unten.',
	appearance: 'Darstellung',
	theme: 'Farbschema',
	themeSystem: 'System',
	themeLight: 'Hell',
	themeDark: 'Dunkel',
	accent: 'Akzentfarbe',
	accentZinc: 'Neutral',
	accentBlue: 'Blau',
	accentGreen: 'Grün',
	accentViolet: 'Violett',
	accentOrange: 'Orange',
	preferencesLocal: 'Nur in diesem Browser gespeichert; der Manager speichert noch keine Benutzereinstellungen.',
	account: 'Konto',
	accountId: 'Konto',
	sessionSince: 'Angemeldet seit',
	addPasskey: 'Passkey hinzufügen',
	addPasskeyHint:
		'Das Registrieren eines Passkeys beendet alle Sitzungen deines Kontos. Dieser Browser erhält automatisch eine neue Sitzung.',
	passkeyLabel: 'Name des Passkeys (optional)',
	passkeyAdded: 'Passkey „{label}“ registriert.',
	passkeyAddedUnnamed: 'Passkey registriert.',
	managerSettings: 'Manager-Einstellungen',

	loginTitle: 'Anmelden',
	loginIntro: 'Melde dich mit einem für diesen Manager gespeicherten Passkey an.',
	loginButton: 'Mit Passkey anmelden',
	loginFailed: 'Anmeldung fehlgeschlagen. Verwende einen für diesen Manager registrierten Passkey.',
	lostPasskey: 'Passkey verloren? Nutze die Kontowiederherstellung.',

	setupTitle: 'Ersten Administrator anlegen',
	setupIntro:
		'Gib das einmalige Setup-Token ein, das der Manager beim Start ausgibt, wähle einen Kontonamen und registriere einen Passkey.',
	setupUnavailable:
		'Die Einrichtung ist nicht mehr möglich: Ein Administrator existiert bereits oder das Setup-Token ist abgelaufen. Melde dich stattdessen an.',
	setupToken: 'Setup-Token',
	setupTokenHint: 'Wird beim Start einmalig in das Manager-Log geschrieben.',
	accountName: 'Kontoname',
	accountNameHint: 'Buchstaben, Ziffern und . _ @ - (bis zu 64 Zeichen).',
	displayName: 'Anzeigename (optional)',
	registerPasskey: 'Passkey registrieren und Administrator anlegen',
	setupWrongToken: 'Das Setup-Token wurde nicht akzeptiert.',
	recoveryCodesTitle: 'Wiederherstellungscodes',
	recoveryCodesIntro:
		'Bewahre diese Codes jetzt sicher auf. Jeder Code kann einmal verwendet werden, um bei Verlust deiner Passkeys wieder Zugang zu erhalten. Sie werden nur dieses eine Mal angezeigt.',
	recoveryCodesMissing:
		'Wiederherstellungscodes konnten nicht erzeugt werden. Dein Administratorkonto funktioniert, hat aber noch keine Wiederherstellungscodes.',
	copyCodes: 'Codes kopieren',
	downloadCodes: 'Als Textdatei herunterladen',
	copied: 'Kopiert.',
	codesStored: 'Ich habe diese Wiederherstellungscodes sicher aufbewahrt.',
	continueToManager: 'Weiter zum Manager',

	recoverTitle: 'Kontowiederherstellung',
	recoverIntro:
		'Löse einen Wiederherstellungscode ein. Deine bisherigen Passkeys werden entfernt und alle Sitzungen beendet; danach registrierst du einen neuen Passkey.',
	recoveryCode: 'Wiederherstellungscode',
	redeemCode: 'Wiederherstellungscode einlösen',
	recoveryFailed: 'Wiederherstellung fehlgeschlagen. Prüfe Kontoname und Code.',
	recoverTokenTitle: 'Wiederherstellungslink',
	recoverTokenIntro:
		'Dieser einmalige Link wurde über die Manager-CLI ausgestellt. Wenn du fortfährst, werden die Passkeys des Kontos {account} entfernt, seine Sitzungen beendet und du kannst einen neuen Passkey registrieren.',
	recoverTokenMissing: 'Dieser Wiederherstellungslink ist unvollständig. Fordere über die Manager-CLI einen neuen an.',
	redeemToken: 'Wiederherstellung fortsetzen',
	enrollTitle: 'Neuen Passkey registrieren',
	enrollIntro: 'Die Wiederherstellung war erfolgreich. Registriere einen neuen Passkey für das Konto {account}.',
	enrollButton: 'Passkey registrieren',
	backToLogin: 'Zurück zur Anmeldung'
};

const messages: Record<Locale, Record<MessageKey, string>> = { en, de };

export function isLocale(value: unknown): value is Locale {
	return typeof value === 'string' && (locales as readonly string[]).includes(value);
}

/** Picks the first supported language from a browser preference list; defaults to English. */
export function detectLocale(preferred: readonly string[]): Locale {
	for (const tag of preferred) {
		const base = tag.toLowerCase().split('-')[0];
		if (isLocale(base)) return base;
	}
	return 'en';
}

/** Keys defined for a locale; used by tests to keep all locales complete. */
export function messageKeys(locale: Locale): string[] {
	return Object.keys(messages[locale]).sort();
}

/** Placeholders like {name} used in a message. */
export function placeholders(locale: Locale, key: MessageKey): string[] {
	return [...messages[locale][key].matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
}

export type Params = Record<string, string | number>;

export function translate(locale: Locale, key: MessageKey, params?: Params): string {
	const text = messages[locale][key];
	if (!params) return text;
	return text.replace(/\{(\w+)\}/g, (m, name: string) => (name in params ? String(params[name]) : m));
}
