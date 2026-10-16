import { useMemo, useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import qrcode from "qrcode-generator";
import { api, type User } from "../api";
import { CodeBox, ErrorBox, Field, errorMessage, useToast } from "../components/ui";
import { SessionsCard, TokensCard } from "./Tokens";
import { PasskeysCard } from "./Passkeys";

export function AccountPage({ user }: { user: User }) {
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Account</h1>
          <div className="sub">
            {user.email} · {user.role}
          </div>
        </div>
      </div>
      <div className="grid-2">
        <PasswordCard />
        <TwoFactorCard user={user} />
      </div>
      <div className="stack" style={{ marginTop: 16 }}>
        <PasskeysCard />
        <SessionsCard />
        <TokensCard user={user} />
      </div>
    </>
  );
}

function PasswordCard() {
  const toast = useToast();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.changePassword(current, next);
      setCurrent("");
      setNext("");
      toast("Password changed. Other devices were signed out.");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="card card-body form" onSubmit={submit}>
      <h2>Password</h2>
      <ErrorBox error={error} />
      <Field label="Current password">
        <input className="input" type="password" value={current} onChange={(e) => setCurrent(e.target.value)} required autoComplete="current-password" />
      </Field>
      <Field label="New password" help="At least 12 characters.">
        <input className="input" type="password" value={next} onChange={(e) => setNext(e.target.value)} minLength={12} required autoComplete="new-password" />
      </Field>
      <div>
        <button className="btn primary" disabled={busy}>
          Change password
        </button>
      </div>
    </form>
  );
}

function QR({ text }: { text: string }) {
  const cells = useMemo(() => {
    const qr = qrcode(0, "M");
    qr.addData(text);
    qr.make();
    const n = qr.getModuleCount();
    const out: { r: number; c: number }[] = [];
    for (let r = 0; r < n; r++) for (let c = 0; c < n; c++) if (qr.isDark(r, c)) out.push({ r, c });
    return { n, out };
  }, [text]);
  return (
    <div className="qr">
      <svg viewBox={`-2 -2 ${cells.n + 4} ${cells.n + 4}`} shapeRendering="crispEdges" role="img" aria-label="QR code">
        {cells.out.map(({ r, c }) => (
          <rect key={`${r}-${c}`} x={c} y={r} width="1" height="1" fill="#000" />
        ))}
      </svg>
    </div>
  );
}

function TwoFactorCard({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [stage, setStage] = useState<"idle" | "password" | "scan" | "codes" | "disable">("idle");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [secret, setSecret] = useState<{ secret: string; uri: string } | null>(null);
  const [codes, setCodes] = useState<string[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const step = async (f: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await f();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const refreshMe = () => qc.invalidateQueries({ queryKey: ["me"] });

  if (stage === "codes") {
    return (
      <div className="card card-body stack">
        <h2>Save your recovery codes</h2>
        <p className="muted" style={{ margin: 0 }}>
          Each code signs you in once if you lose your phone. Store them somewhere safe; they are not shown again.
        </p>
        <div className="recovery-codes">
          {codes.map((c) => (
            <span key={c}>{c}</span>
          ))}
        </div>
        <CodeBox text={codes.join("\n")} />
        <div>
          <button
            className="btn primary"
            onClick={() => {
              setStage("idle");
              refreshMe();
            }}
          >
            I saved them
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="card card-body form">
      <div className="row">
        <h2>Two-factor authentication</h2>
        {user.totp_enabled ? <span className="badge ok">on</span> : <span className="badge warn">off</span>}
      </div>
      <ErrorBox error={error} />

      {stage === "idle" && (
        <>
          <p className="muted" style={{ margin: 0 }}>
            {user.totp_enabled
              ? "Signing in requires a code from your authenticator app."
              : "Protect your account with a code from an authenticator app (1Password, Bitwarden, Google Authenticator, Aegis…)."}
          </p>
          <div>
            {user.totp_enabled ? (
              <button className="btn danger" onClick={() => setStage("disable")}>
                Turn off
              </button>
            ) : (
              <button className="btn primary" onClick={() => setStage("password")}>
                Set up
              </button>
            )}
          </div>
        </>
      )}

      {stage === "password" && (
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault();
            step(async () => {
              setSecret(await api.totpSetup(password));
              setPassword("");
              setStage("scan");
            });
          }}
        >
          <Field label="Confirm your password">
            <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
          </Field>
          <div className="row">
            <button className="btn primary" disabled={busy}>
              Continue
            </button>
            <button type="button" className="btn ghost" onClick={() => setStage("idle")}>
              Cancel
            </button>
          </div>
        </form>
      )}

      {stage === "scan" && secret && (
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault();
            step(async () => {
              const r = await api.totpEnable(code);
              setCodes(r.recovery_codes);
              setCode("");
              setStage("codes");
              toast("Two-factor authentication is on");
            });
          }}
        >
          <p className="muted" style={{ margin: 0 }}>
            Scan the code with your authenticator app, or enter the secret manually.
          </p>
          <QR text={secret.uri} />
          <CodeBox text={secret.secret} />
          <Field label="6-digit code from the app">
            <input className="input mono" value={code} onChange={(e) => setCode(e.target.value)} required autoFocus inputMode="numeric" autoComplete="one-time-code" />
          </Field>
          <div className="row">
            <button className="btn primary" disabled={busy}>
              Turn on
            </button>
            <button type="button" className="btn ghost" onClick={() => setStage("idle")}>
              Cancel
            </button>
          </div>
        </form>
      )}

      {stage === "disable" && (
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault();
            step(async () => {
              await api.totpDisable(password, code);
              setPassword("");
              setCode("");
              setStage("idle");
              refreshMe();
              toast("Two-factor authentication is off");
            });
          }}
        >
          <Field label="Password">
            <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
          </Field>
          <Field label="Code from the app or a recovery code">
            <input className="input mono" value={code} onChange={(e) => setCode(e.target.value)} required autoComplete="one-time-code" />
          </Field>
          <div className="row">
            <button className="btn danger solid" disabled={busy}>
              Turn off
            </button>
            <button type="button" className="btn ghost" onClick={() => setStage("idle")}>
              Cancel
            </button>
          </div>
        </form>
      )}
      {error !== null && stage === "idle" && <div className="small faint">{errorMessage(error)}</div>}
    </div>
  );
}
