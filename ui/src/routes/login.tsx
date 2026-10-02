import { Button, Card, Description, Input, Label, Spinner, Tabs, TextField } from "@heroui/react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { toast } from "sonner";
import PhoneIcon from "~icons/gravity-ui/person";
import QrIcon from "~icons/gravity-ui/qr-code";
import ShieldIcon from "~icons/gravity-ui/shield-check";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { newIdempotencyKey } from "@/features/shared/idempotency";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import { currentUserQueryOptions } from "@/auth/queries";

type Step = "phone" | "code" | "password";
type Flow = {
  flowId: string;
  expiresAt: string;
  passwordRequired?: boolean;
  state?: string;
  qrUrl?: string;
  qrExpiresAt?: string;
};
type CookieSession = { authenticated: true; expiresAt: string };

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>) => ({
    redirect:
      typeof search.redirect === "string" && search.redirect.startsWith("/")
        ? search.redirect
        : "/files",
  }),
  component: LoginPage,
});

function isSession(value: unknown): value is CookieSession {
  return Boolean(value && typeof value === "object" && "authenticated" in value);
}

function LoginPage() {
  const navigate = useNavigate();
  const { t } = useI18n();
  const { redirect } = Route.useSearch();
  const [method, setMethod] = useState<"phone" | "qr">("phone");
  const [step, setStep] = useState<Step>("phone");
  const [flowId, setFlowId] = useState("");
  const [phone, setPhone] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [qrUrl, setQrUrl] = useState("");
  const [qrExpiry, setQrExpiry] = useState("");
  const [clock, setClock] = useState(() => Date.now());

  const startPhone = $api.useMutation("post", "/v1/auth/telegram/start");
  const verifyCode = $api.useMutation("post", "/v1/auth/cookie/telegram/verify-code");
  const verifyPassword = $api.useMutation("post", "/v1/auth/cookie/telegram/verify-password");
  const startQr = $api.useMutation("post", "/v1/auth/telegram/qr/start");
  const pollQr = $api.useMutation("post", "/v1/auth/cookie/telegram/qr/poll");
  const pending = startPhone.isPending || verifyCode.isPending || verifyPassword.isPending;

  const finish = async () => {
    const query = currentUserQueryOptions();
    const qc = getQueryClient();
    await qc.invalidateQueries({ queryKey: query.queryKey });
    await qc.ensureQueryData(query);
    toast.success(t("routes.login.toast.signedIn"));
    await navigate({ to: redirect, replace: true });
  };

  const submitPhone = async () => {
    try {
      if (step === "phone") {
        const result = (await startPhone.mutateAsync({
          params: { header: { "Idempotency-Key": newIdempotencyKey() } },
          body: { phoneNumber: phone.trim() },
        })) as Flow;
        setFlowId(result.flowId);
        setStep(result.passwordRequired ? "password" : "code");
        return;
      }
      if (step === "code") {
        const result = await verifyCode.mutateAsync({
          params: { header: { "Idempotency-Key": newIdempotencyKey() } },
          body: { flowId, code: code.trim() },
        });
        if (isSession(result)) await finish();
        else setStep("password");
        return;
      }
      const result = await verifyPassword.mutateAsync({
        params: { header: { "Idempotency-Key": newIdempotencyKey() } },
        body: { flowId, password },
      });
      if (isSession(result)) await finish();
    } catch (error) {
      toast.error(t("routes.login.toast.signInFailed"), { description: userMessage(error) });
    }
  };

  useEffect(() => {
    if (method !== "qr") return;
    let active = true;
    let timer = 0;
    let polling = false;
    void startQr
      .mutateAsync({ params: { header: { "Idempotency-Key": newIdempotencyKey() } } })
      .then((result) => {
        if (!active) return;
        const flow = result as Flow;
        setFlowId(flow.flowId);
        setQrUrl(flow.qrUrl ?? "");
        setQrExpiry(flow.qrExpiresAt ?? flow.expiresAt);
        timer = window.setInterval(async () => {
          // A Telegram round trip regularly outlives the interval, so skip the
          // tick instead of stacking overlapping polls: every poll exports a new
          // login token, and the extra exports only delay the scan.
          if (polling) return;
          polling = true;
          try {
            const next = await pollQr.mutateAsync({
              params: { header: { "Idempotency-Key": newIdempotencyKey() } },
              body: { flowId: flow.flowId },
            });
            if (!active) return;
            if (isSession(next)) {
              window.clearInterval(timer);
              await finish();
              return;
            }
            const state = next as Flow;
            if (state.state === "password_required") {
              window.clearInterval(timer);
              setMethod("phone");
              setStep("password");
              return;
            }
            if (state.qrUrl) setQrUrl(state.qrUrl);
            if (state.qrExpiresAt) setQrExpiry(state.qrExpiresAt);
          } catch (error) {
            window.clearInterval(timer);
            toast.error(t("routes.login.toast.qrStopped"), { description: userMessage(error) });
          } finally {
            polling = false;
          }
        }, 2500);
      })
      .catch((error) =>
        toast.error(t("routes.login.toast.qrStartFailed"), { description: userMessage(error) }),
      );
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [method]);

  // Each step owns exactly one input, and browsers like to replay the text of a
  // field that just disappeared into the password field that appeared in its
  // place. Dropping the previous step's value keeps the two apart.
  useEffect(() => {
    if (step === "password") setCode("");
    if (step !== "password") setPassword("");
  }, [step]);

  // Tick once a second so the QR screen can show a countdown instead of a
  // wall-clock expiry that means nothing while the code rotates every 30s.
  useEffect(() => {
    if (!qrExpiry) return;
    setClock(Date.now());
    const tick = window.setInterval(() => setClock(Date.now()), 1000);
    return () => window.clearInterval(tick);
  }, [qrExpiry]);

  const qrSecondsLeft = qrExpiry
    ? Math.max(0, Math.ceil((new Date(qrExpiry).getTime() - clock) / 1000))
    : null;

  return (
    <main className="grid min-h-dvh bg-background text-foreground lg:grid-cols-[minmax(0,1.1fr)_minmax(24rem,0.9fr)]">
      <section className="hidden border-r border-border bg-sidebar/70 p-12 lg:flex lg:flex-col lg:justify-between">
        <img
          src="/images/apple-touch-icon.png"
          alt={t("common.app.name")}
          width={44}
          height={44}
          className="size-11 rounded-xl"
        />
        <div className="my-auto max-w-xl">
          <p className="mb-3 text-xs font-medium uppercase tracking-[0.16em] text-accent">
            {t("common.app.name")}
          </p>
          <h1 className="text-4xl font-semibold tracking-tight">{t("routes.login.hero.title")}</h1>
          <p className="mt-4 max-w-lg text-sm leading-6 text-muted">
            {t("routes.login.hero.description")}
          </p>
        </div>
      </section>
      <section className="flex items-center justify-center p-4 sm:p-8 lg:p-12">
        <Card className="w-full max-w-md border border-border bg-surface/90 shadow-xl">
          <Card.Header className="block px-6 pt-6">
            <Card.Title>{t("routes.login.title")}</Card.Title>
            <Card.Description>{t("routes.login.description")}</Card.Description>
          </Card.Header>
          <Card.Content className="space-y-5 px-6 pb-6">
            <Tabs
              selectedKey={method}
              onSelectionChange={(key) => {
                setMethod(key as "phone" | "qr");
                setStep("phone");
              }}
            >
              <Tabs.ListContainer>
                <Tabs.List aria-label={t("routes.login.method.label")}>
                  <Tabs.Tab id="phone">
                    <PhoneIcon className="size-4" /> {t("routes.login.method.phone")}
                  </Tabs.Tab>
                  <Tabs.Tab id="qr">
                    <QrIcon className="size-4" /> {t("routes.login.method.qr")}
                  </Tabs.Tab>
                </Tabs.List>
              </Tabs.ListContainer>
              <Tabs.Panel id="phone" className="space-y-4 pt-4">
                {/* The step is a real form so Enter in its only input submits it,
                    exactly like the public share password form. */}
                <form
                  className="space-y-4"
                  onSubmit={(event) => {
                    event.preventDefault();
                    void submitPhone();
                  }}
                >
                  {/* Without autocomplete hints browsers classify the code field as
                    a username and replay it into the password field that appears
                    next; one-time-code/current-password keep the two apart. */}
                  {step === "phone" && (
                    <TextField className="grid gap-1">
                      <Label>{t("routes.login.phone.label")}</Label>
                      <Input
                        autoFocus
                        autoComplete="tel"
                        placeholder="+12025550123"
                        value={phone}
                        onChange={(event) => setPhone(event.target.value)}
                      />
                      <Description>{t("routes.login.phone.description")}</Description>
                    </TextField>
                  )}
                  {step === "code" && (
                    <TextField className="grid gap-1">
                      <Label>{t("routes.login.code.label")}</Label>
                      <Input
                        autoFocus
                        autoComplete="one-time-code"
                        inputMode="numeric"
                        value={code}
                        onChange={(event) => setCode(event.target.value)}
                      />
                    </TextField>
                  )}
                  {step === "password" && (
                    <TextField className="grid gap-1">
                      <Label>{t("routes.login.password.label")}</Label>
                      <Input
                        autoFocus
                        autoComplete="current-password"
                        type="password"
                        value={password}
                        onChange={(event) => setPassword(event.target.value)}
                      />
                    </TextField>
                  )}
                  <Button
                    type="submit"
                    className="w-full"
                    isDisabled={
                      pending ||
                      (step === "phone"
                        ? !phone.trim()
                        : step === "code"
                          ? !code.trim()
                          : !password)
                    }
                  >
                    {pending ? <Spinner size="sm" /> : <ShieldIcon className="size-4" />}
                    {step === "phone"
                      ? t("routes.login.action.sendCode")
                      : t("routes.login.action.verify")}
                  </Button>
                  {step !== "phone" && (
                    <Button
                      type="button"
                      variant="ghost"
                      className="w-full"
                      onPress={() => {
                        setStep("phone");
                        setFlowId("");
                        setCode("");
                        setPassword("");
                      }}
                    >
                      {t("routes.login.action.restart")}
                    </Button>
                  )}
                </form>
              </Tabs.Panel>
              <Tabs.Panel id="qr" className="space-y-4 pt-4">
                <div className="grid min-h-72 place-items-center rounded-xl border border-border bg-white p-5 text-black">
                  {qrUrl ? (
                    <QRCodeSVG value={qrUrl} size={220} aria-label={t("routes.login.qr.label")} />
                  ) : (
                    <Spinner size="lg" />
                  )}
                </div>
                <div className="text-center">
                  <p className="font-medium">{t("routes.login.qr.scan")}</p>
                  <p className="mt-1 text-xs text-muted">{t("routes.login.qr.steps")}</p>
                  <p className="mt-2 text-xs text-muted">
                    {qrSecondsLeft === null
                      ? t("routes.login.qr.preparing")
                      : qrSecondsLeft > 0
                        ? t("routes.login.qr.expiresIn", { seconds: qrSecondsLeft })
                        : t("routes.login.qr.expired")}
                  </p>
                </div>
              </Tabs.Panel>
            </Tabs>
          </Card.Content>
        </Card>
      </section>
    </main>
  );
}
