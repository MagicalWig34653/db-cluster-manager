// WebAuthn JSON <-> ArrayBuffer conversion.
//
// The manager (go-webauthn) sends the `publicKey` member of the creation or
// request options as JSON with base64url-encoded binary fields, matching the
// WebAuthn Level 3 PublicKeyCredentialCreationOptionsJSON and
// PublicKeyCredentialRequestOptionsJSON shapes. It expects the credential back
// in the shape produced by PublicKeyCredential.toJSON().
//
// Browsers that implement the Level 3 helpers
// (PublicKeyCredential.parseCreationOptionsFromJSON, parseRequestOptionsFromJSON
// and PublicKeyCredential.prototype.toJSON) are used directly; otherwise a
// small fallback performs the same conversion.

export type Base64URLString = string;

export interface PublicKeyCredentialDescriptorJSON {
	id: Base64URLString;
	type: string;
	transports?: string[];
}

export interface PublicKeyCredentialUserEntityJSON {
	id: Base64URLString;
	name: string;
	displayName: string;
}

export interface PublicKeyCredentialCreationOptionsJSON {
	rp: { id?: string; name: string };
	user: PublicKeyCredentialUserEntityJSON;
	challenge: Base64URLString;
	pubKeyCredParams: { type: string; alg: number }[];
	timeout?: number;
	excludeCredentials?: PublicKeyCredentialDescriptorJSON[];
	authenticatorSelection?: Record<string, unknown>;
	hints?: string[];
	attestation?: string;
	attestationFormats?: string[];
	extensions?: Record<string, unknown>;
}

export interface PublicKeyCredentialRequestOptionsJSON {
	challenge: Base64URLString;
	timeout?: number;
	rpId?: string;
	allowCredentials?: PublicKeyCredentialDescriptorJSON[];
	userVerification?: string;
	hints?: string[];
	extensions?: Record<string, unknown>;
}

export interface RegistrationResponseJSON {
	id: Base64URLString;
	rawId: Base64URLString;
	type: string;
	authenticatorAttachment?: string | null;
	clientExtensionResults: Record<string, unknown>;
	response: {
		clientDataJSON: Base64URLString;
		attestationObject: Base64URLString;
		transports?: string[];
		authenticatorData?: Base64URLString;
		publicKey?: Base64URLString;
		publicKeyAlgorithm?: number;
	};
}

export interface AuthenticationResponseJSON {
	id: Base64URLString;
	rawId: Base64URLString;
	type: string;
	authenticatorAttachment?: string | null;
	clientExtensionResults: Record<string, unknown>;
	response: {
		clientDataJSON: Base64URLString;
		authenticatorData: Base64URLString;
		signature: Base64URLString;
		userHandle?: Base64URLString | null;
	};
}

export type CredentialJSON = RegistrationResponseJSON | AuthenticationResponseJSON;

/** The subset of the PublicKeyCredential interface object that is feature-detected. */
export interface PublicKeyCredentialStatics {
	parseCreationOptionsFromJSON?: (o: PublicKeyCredentialCreationOptionsJSON) => PublicKeyCredentialCreationOptions;
	parseRequestOptionsFromJSON?: (o: PublicKeyCredentialRequestOptionsJSON) => PublicKeyCredentialRequestOptions;
}

function nativeStatics(): PublicKeyCredentialStatics | undefined {
	return (globalThis as { PublicKeyCredential?: PublicKeyCredentialStatics }).PublicKeyCredential;
}

export function base64urlToBuffer(value: string): ArrayBuffer {
	if (!/^[A-Za-z0-9_-]*={0,2}$/.test(value)) throw new TypeError('invalid base64url');
	const b64 = value.replace(/=+$/, '').replace(/-/g, '+').replace(/_/g, '/');
	const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4);
	const bin = atob(padded);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out.buffer;
}

export function bufferToBase64url(value: ArrayBuffer | ArrayBufferView): string {
	const bytes =
		value instanceof ArrayBuffer
			? new Uint8Array(value)
			: new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
	let bin = '';
	for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
	return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function descriptors(list: PublicKeyCredentialDescriptorJSON[] | undefined) {
	return list?.map((d) => ({ ...d, id: base64urlToBuffer(d.id) })) as PublicKeyCredentialDescriptor[] | undefined;
}

function withoutUndefined<T extends object>(o: T): T {
	return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as T;
}

/** Fallback for PublicKeyCredential.parseCreationOptionsFromJSON. */
export function parseCreationOptionsFallback(
	json: PublicKeyCredentialCreationOptionsJSON
): PublicKeyCredentialCreationOptions {
	return withoutUndefined({
		...json,
		challenge: base64urlToBuffer(json.challenge),
		user: { ...json.user, id: base64urlToBuffer(json.user.id) },
		excludeCredentials: descriptors(json.excludeCredentials)
	}) as unknown as PublicKeyCredentialCreationOptions;
}

/** Fallback for PublicKeyCredential.parseRequestOptionsFromJSON. */
export function parseRequestOptionsFallback(
	json: PublicKeyCredentialRequestOptionsJSON
): PublicKeyCredentialRequestOptions {
	return withoutUndefined({
		...json,
		challenge: base64urlToBuffer(json.challenge),
		allowCredentials: descriptors(json.allowCredentials)
	}) as unknown as PublicKeyCredentialRequestOptions;
}

export function creationOptionsFromJSON(
	json: PublicKeyCredentialCreationOptionsJSON,
	statics: PublicKeyCredentialStatics | undefined = nativeStatics()
): PublicKeyCredentialCreationOptions {
	if (typeof statics?.parseCreationOptionsFromJSON === 'function') return statics.parseCreationOptionsFromJSON(json);
	return parseCreationOptionsFallback(json);
}

export function requestOptionsFromJSON(
	json: PublicKeyCredentialRequestOptionsJSON,
	statics: PublicKeyCredentialStatics | undefined = nativeStatics()
): PublicKeyCredentialRequestOptions {
	if (typeof statics?.parseRequestOptionsFromJSON === 'function') return statics.parseRequestOptionsFromJSON(json);
	return parseRequestOptionsFallback(json);
}

/** Structural view of a PublicKeyCredential, so tests can pass plain objects. */
export interface CredentialLike {
	id: string;
	rawId: ArrayBuffer;
	type: string;
	authenticatorAttachment?: string | null;
	response: {
		clientDataJSON: ArrayBuffer;
		attestationObject?: ArrayBuffer;
		getTransports?: () => string[];
		getAuthenticatorData?: () => ArrayBuffer;
		getPublicKey?: () => ArrayBuffer | null;
		getPublicKeyAlgorithm?: () => number;
		authenticatorData?: ArrayBuffer;
		signature?: ArrayBuffer;
		userHandle?: ArrayBuffer | null;
	};
	getClientExtensionResults?: () => Record<string, unknown>;
	toJSON?: () => unknown;
}

/** Fallback for PublicKeyCredential.prototype.toJSON. */
export function credentialToJSONFallback(cred: CredentialLike): CredentialJSON {
	const r = cred.response;
	const base = {
		id: cred.id,
		rawId: bufferToBase64url(cred.rawId),
		type: cred.type,
		authenticatorAttachment: cred.authenticatorAttachment ?? null,
		clientExtensionResults: cred.getClientExtensionResults?.() ?? {}
	};
	if (r.attestationObject) {
		const pk = r.getPublicKey?.();
		return {
			...base,
			response: withoutUndefined({
				clientDataJSON: bufferToBase64url(r.clientDataJSON),
				attestationObject: bufferToBase64url(r.attestationObject),
				transports: r.getTransports?.() ?? [],
				authenticatorData: r.getAuthenticatorData ? bufferToBase64url(r.getAuthenticatorData()) : undefined,
				publicKey: pk ? bufferToBase64url(pk) : undefined,
				publicKeyAlgorithm: r.getPublicKeyAlgorithm?.()
			})
		};
	}
	if (!r.authenticatorData || !r.signature) throw new TypeError('unsupported credential response');
	return {
		...base,
		response: {
			clientDataJSON: bufferToBase64url(r.clientDataJSON),
			authenticatorData: bufferToBase64url(r.authenticatorData),
			signature: bufferToBase64url(r.signature),
			userHandle: r.userHandle ? bufferToBase64url(r.userHandle) : null
		}
	};
}

export function credentialToJSON(cred: CredentialLike): CredentialJSON {
	if (typeof cred.toJSON === 'function') return cred.toJSON() as CredentialJSON;
	return credentialToJSONFallback(cred);
}

/** Why a passkey ceremony in the browser failed, without exposing browser messages verbatim. */
export type CeremonyFailure = 'unsupported' | 'insecure' | 'cancelled' | 'exists' | 'failed';

export class CeremonyError extends Error {
	constructor(readonly reason: CeremonyFailure) {
		super(`passkey ceremony: ${reason}`);
	}
}

function classify(e: unknown): CeremonyError {
	const name = (e as { name?: string })?.name;
	if (name === 'NotAllowedError' || name === 'AbortError') return new CeremonyError('cancelled');
	if (name === 'InvalidStateError') return new CeremonyError('exists');
	if (name === 'SecurityError') return new CeremonyError('insecure');
	if (name === 'NotSupportedError') return new CeremonyError('unsupported');
	return new CeremonyError('failed');
}

function ensureAvailable() {
	if (typeof window !== 'undefined' && !window.isSecureContext) throw new CeremonyError('insecure');
	if (typeof navigator === 'undefined' || !navigator.credentials || !nativeStatics())
		throw new CeremonyError('unsupported');
}

/** Runs navigator.credentials.create with the server's options and returns the JSON to send back. */
export async function createPasskey(options: unknown): Promise<CredentialJSON> {
	ensureAvailable();
	let cred: Credential | null;
	try {
		cred = await navigator.credentials.create({
			publicKey: creationOptionsFromJSON(options as PublicKeyCredentialCreationOptionsJSON)
		});
	} catch (e) {
		throw classify(e);
	}
	if (!cred) throw new CeremonyError('cancelled');
	return credentialToJSON(cred as unknown as CredentialLike);
}

/** Runs navigator.credentials.get (discoverable login) and returns the JSON to send back. */
export async function getPasskey(options: unknown): Promise<CredentialJSON> {
	ensureAvailable();
	let cred: Credential | null;
	try {
		cred = await navigator.credentials.get({
			publicKey: requestOptionsFromJSON(options as PublicKeyCredentialRequestOptionsJSON)
		});
	} catch (e) {
		throw classify(e);
	}
	if (!cred) throw new CeremonyError('cancelled');
	return credentialToJSON(cred as unknown as CredentialLike);
}
