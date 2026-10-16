import { useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { ErrorBox, Field, Logo } from "../components/ui";
import { passkeysSupported, signInWithPasskey } from "../components/passkey";

export function SetupPage() {
  const qc = useQueryClient();
  const [form, setForm] = useState({ setup_token: "", email: "", name: "", password: "" });
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const { user } = await api.setup(form);
      qc.setQueryData(["setup"], { required: false });
      qc.setQueryData(["me"], { user });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <form className="card card-body auth-card form" onSubmit={submit}>
        <div className="brand">
          <Logo size={28} /> Basalt
        </div>
        <div>
          <h1>Welcome</h1>
          <p className="muted">Create the owner account for this installation.</p>
        </div>
        <ErrorBox error={error} />
        <Field label="Setup token" help="Printed in the server log on first start.">
          <input className="input mono" value={form.setup_token} onChange={set("setup_token")} required autoFocus />
        </Field>
        <Field label="Name">
          <input className="input" value={form.name} onChange={set("name")} required autoComplete="name" />
        </Field>
        <Field label="Email">
          <input className="input" type="email" value={form.email} onChange={set("email")} required autoComplete="email" />
        </Field>
        <Field label="Password" help="At least 12 characters. A passphrase works well.">
          <input
            className="input"
            type="password"
            value={form.password}
            onChange={set("password")}
            required
            minLength={12}
            autoComplete="new-password"
          />
        </Field>
        <button className="btn primary" disabled={busy}>
          {busy ? "Creating…" : "Create owner account"}
        </button>
      </form>
    </div>
  );
}

export function LoginPage() {
  const qc = useQueryClient();
  const [step, setStep] = useState<"password" | "mfa">("password");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const passkey = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = await signInWithPasskey();
      qc.setQueryData(["me"], { user: res.user });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      if (step === "password") {
        const res = await api.login(email, password);
        if (res.mfa_required) {
          setStep("mfa");
          setPassword("");
        } else {
          qc.setQueryData(["me"], { user: res.user });
        }
      } else {
        const res = await api.mfa(code);
        qc.setQueryData(["me"], { user: res.user });
      }
    } catch (err) {
      setError(err);
      if (step === "mfa") setCode("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <form className="card card-body auth-card form" onSubmit={submit}>
        <div className="brand">
          <Logo size={28} /> Basalt
        </div>
        <ErrorBox error={error} />
        {step === "password" ? (
          <>
            <Field label="Email">
              <input
                className="input"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                autoFocus
                autoComplete="username"
              />
            </Field>
            <Field label="Password">
              <input
                className="input"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                autoComplete="current-password"
              />
            </Field>
            <button className="btn primary" disabled={busy}>
              {busy ? "Signing in…" : "Sign in"}
            </button>
            {passkeysSupported() && (
              <button type="button" className="btn" onClick={passkey} disabled={busy}>
                Sign in with a passkey
              </button>
            )}
          </>
        ) : (
          <>
            <div>
              <h2>Two-factor authentication</h2>
              <p className="muted">Enter the 6-digit code from your authenticator app, or a recovery code.</p>
            </div>
            <Field label="Code">
              <input
                className="input mono"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                required
                autoFocus
                inputMode="text"
                autoComplete="one-time-code"
                placeholder="123456"
              />
            </Field>
            <button className="btn primary" disabled={busy}>
              {busy ? "Verifying…" : "Verify"}
            </button>
            <button type="button" className="btn ghost" onClick={() => setStep("password")}>
              Back
            </button>
          </>
        )}
      </form>
    </div>
  );
}
