import { api } from "../api";

// The browser side of passkeys: the server speaks JSON with binary fields
// in base64url; the WebAuthn API wants ArrayBuffers.

const toBuf = (s: string) => {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
  return Uint8Array.from(b, (c) => c.charCodeAt(0)).buffer;
};
const toB64 = (buf: ArrayBuffer) =>
  btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

export const passkeysSupported = () => typeof window !== "undefined" && !!window.PublicKeyCredential && window.isSecureContext;

// friendly turns the browser's errors into something to show.
function friendly(e: unknown): Error {
  if (e instanceof DOMException) {
    if (e.name === "NotAllowedError") return new Error("Cancelled, or the passkey prompt timed out.");
    if (e.name === "InvalidStateError") return new Error("This device already holds a passkey for your account.");
    if (e.name === "SecurityError") return new Error("Passkeys need the panel to be opened by its domain name over HTTPS.");
  }
  return e instanceof Error ? e : new Error(String(e));
}

// createPasskey adds a passkey to the signed-in account.
export async function createPasskey(password: string, name: string) {
  const { options: o } = await api.passkeyOptions(password);
  let cred: PublicKeyCredential;
  try {
    cred = (await navigator.credentials.create({
      publicKey: {
        ...o,
        challenge: toBuf(o.challenge),
        user: { ...o.user, id: toBuf(o.user.id) },
        excludeCredentials: o.excludeCredentials.map((c: { id: string; type: "public-key" }) => ({ ...c, id: toBuf(c.id) })),
      },
    })) as PublicKeyCredential;
  } catch (e) {
    throw friendly(e);
  }
  const r = cred.response as AuthenticatorAttestationResponse;
  return api.addPasskey({ name, client_data_json: toB64(r.clientDataJSON), attestation_object: toB64(r.attestationObject) });
}

// signInWithPasskey signs in with any passkey of this panel.
export async function signInWithPasskey() {
  const { challenge_id, options: o } = await api.passkeyLoginOptions();
  let cred: PublicKeyCredential;
  try {
    cred = (await navigator.credentials.get({ publicKey: { ...o, challenge: toBuf(o.challenge), allowCredentials: [] } })) as PublicKeyCredential;
  } catch (e) {
    throw friendly(e);
  }
  const r = cred.response as AuthenticatorAssertionResponse;
  return api.passkeyLogin({
    challenge_id,
    credential_id: toB64(cred.rawId),
    client_data_json: toB64(r.clientDataJSON),
    authenticator_data: toB64(r.authenticatorData),
    signature: toB64(r.signature),
    user_handle: r.userHandle ? toB64(r.userHandle) : "",
  });
}
