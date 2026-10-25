import { describe, expect, it } from 'vitest';
import {
	base64urlToBuffer,
	bufferToBase64url,
	creationOptionsFromJSON,
	credentialToJSON,
	credentialToJSONFallback,
	requestOptionsFromJSON,
	type CredentialLike,
	type PublicKeyCredentialCreationOptionsJSON,
	type PublicKeyCredentialRequestOptionsJSON
} from './webauthn';

const bytes = (...b: number[]) => new Uint8Array(b).buffer;
const arr = (b: ArrayBuffer | BufferSource) => Array.from(new Uint8Array(b as ArrayBuffer));

// Shaped like go-webauthn's marshalled creation.Response (the publicKey member).
const creation: PublicKeyCredentialCreationOptionsJSON = {
	rp: { name: 'cluster-manager', id: 'localhost' },
	user: { name: 'admin', displayName: 'Admin', id: 'AQID' },
	challenge: 'q83vEjRWeJA',
	pubKeyCredParams: [
		{ type: 'public-key', alg: -7 },
		{ type: 'public-key', alg: -257 }
	],
	timeout: 300000,
	excludeCredentials: [{ type: 'public-key', id: '-_8' }],
	authenticatorSelection: { residentKey: 'required', requireResidentKey: true, userVerification: 'required' },
	attestation: 'none'
};

const request: PublicKeyCredentialRequestOptionsJSON = {
	challenge: 'AAEC_w',
	timeout: 300000,
	rpId: 'localhost',
	userVerification: 'required'
};

describe('base64url', () => {
	it('round-trips arbitrary bytes without padding', () => {
		const all = new Uint8Array(256).map((_, i) => i);
		const s = bufferToBase64url(all);
		expect(s).not.toMatch(/[+/=]/);
		expect(arr(base64urlToBuffer(s))).toEqual(Array.from(all));
	});
	it('accepts padded input and typed-array views', () => {
		expect(arr(base64urlToBuffer('AQI='))).toEqual([1, 2]);
		const view = new Uint8Array([9, 1, 2, 9]).subarray(1, 3);
		expect(bufferToBase64url(view)).toBe('AQI');
	});
	it('rejects standard base64 alphabet characters', () => {
		expect(() => base64urlToBuffer('a+b/')).toThrow(TypeError);
	});
});

describe('options from JSON (fallback)', () => {
	it('decodes the binary fields of creation options', () => {
		const o = creationOptionsFromJSON(creation, undefined);
		expect(arr(o.challenge)).toEqual([0xab, 0xcd, 0xef, 0x12, 0x34, 0x56, 0x78, 0x90]);
		expect(arr(o.user.id)).toEqual([1, 2, 3]);
		expect(arr(o.excludeCredentials![0].id)).toEqual([0xfb, 0xff]);
		expect(o.user.name).toBe('admin');
		expect(o.rp).toEqual({ name: 'cluster-manager', id: 'localhost' });
		expect(o.authenticatorSelection?.residentKey).toBe('required');
		expect(o.pubKeyCredParams).toHaveLength(2);
		// The input is not mutated.
		expect(creation.challenge).toBe('q83vEjRWeJA');
	});
	it('decodes request options and omits absent lists', () => {
		const o = requestOptionsFromJSON(request, {});
		expect(arr(o.challenge)).toEqual([0, 1, 2, 255]);
		expect('allowCredentials' in o).toBe(false);
		expect(o.userVerification).toBe('required');
		const withAllow = requestOptionsFromJSON({ ...request, allowCredentials: [{ type: 'public-key', id: 'AQ' }] }, {});
		expect(arr(withAllow.allowCredentials![0].id)).toEqual([1]);
	});
	it('prefers the native parser when present', () => {
		const marker = { native: true } as unknown as PublicKeyCredentialCreationOptions;
		const o = creationOptionsFromJSON(creation, { parseCreationOptionsFromJSON: () => marker });
		expect(o).toBe(marker);
	});
});

describe('credential to JSON', () => {
	const registration: CredentialLike = {
		id: 'AQI',
		rawId: bytes(1, 2),
		type: 'public-key',
		authenticatorAttachment: 'platform',
		response: {
			clientDataJSON: bytes(123, 125),
			attestationObject: bytes(0xa0),
			getTransports: () => ['internal', 'hybrid'],
			getAuthenticatorData: () => bytes(5),
			getPublicKey: () => bytes(6),
			getPublicKeyAlgorithm: () => -7
		},
		getClientExtensionResults: () => ({ credProps: { rk: true } })
	};

	it('encodes a registration response like toJSON()', () => {
		expect(credentialToJSONFallback(registration)).toEqual({
			id: 'AQI',
			rawId: 'AQI',
			type: 'public-key',
			authenticatorAttachment: 'platform',
			clientExtensionResults: { credProps: { rk: true } },
			response: {
				clientDataJSON: 'e30',
				attestationObject: 'oA',
				transports: ['internal', 'hybrid'],
				authenticatorData: 'BQ',
				publicKey: 'Bg',
				publicKeyAlgorithm: -7
			}
		});
	});

	it('encodes an assertion response, keeping a null user handle explicit', () => {
		const assertion: CredentialLike = {
			id: 'AQI',
			rawId: bytes(1, 2),
			type: 'public-key',
			response: { clientDataJSON: bytes(1), authenticatorData: bytes(2), signature: bytes(3), userHandle: null }
		};
		expect(credentialToJSONFallback(assertion)).toEqual({
			id: 'AQI',
			rawId: 'AQI',
			type: 'public-key',
			authenticatorAttachment: null,
			clientExtensionResults: {},
			response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw', userHandle: null }
		});
		const withHandle = credentialToJSONFallback({
			...assertion,
			response: { ...assertion.response, userHandle: bytes(1, 2, 3) }
		});
		expect((withHandle.response as { userHandle: string }).userHandle).toBe('AQID');
	});

	it('uses native toJSON when the credential provides it', () => {
		const native = { native: true };
		expect(credentialToJSON({ ...registration, toJSON: () => native })).toBe(native);
	});

	it('rejects responses that are neither attestation nor assertion', () => {
		expect(() =>
			credentialToJSONFallback({ id: 'x', rawId: bytes(1), type: 'public-key', response: { clientDataJSON: bytes(1) } })
		).toThrow(TypeError);
	});
});
