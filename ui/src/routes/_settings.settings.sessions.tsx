import { Button, Chip, Spinner } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { ConfirmDialog } from "@/components/dialogs/confirm-dialog";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import TrashIcon from "~icons/gravity-ui/trash-bin";

export const Route = createFileRoute("/_settings/settings/sessions")({
  component: SessionsSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function SessionsSettings() {
  const { t } = useI18n();
  const [revokeSessionId, setRevokeSessionId] = useState<string | null>(null);
  const query = $api.useSuspenseQuery(
    "get",
    "/v1/sessions",
    { params: { query: { limit: 200 } } },
    { staleTime: 20_000 },
  );
  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/sessions").queryKey,
    });
  const revoke = $api.useMutation("delete", "/v1/sessions/{sessionId}", {
    onSuccess: () => {
      setRevokeSessionId(null);
      void refresh();
      toast.success(t("settings.sessions.toast.revoked"));
    },
    onError: (error) => {
      toast.error(t("settings.sessions.toast.revokeFailed"), { description: userMessage(error) });
    },
  });

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.sessions.title")}
        description={t("settings.sessions.description")}
      />
      <SettingsSection
        title={t("settings.sessions.active.section")}
        description={t("settings.sessions.active.description")}
      >
        {query.data.items.length ? (
          query.data.items.map((session) => (
            <SettingsRow
              key={session.id}
              label={
                session.current ? t("settings.sessions.current") : t("settings.sessions.other")
              }
              description={t("settings.sessions.row.description", {
                created: new Date(session.createdAt).toLocaleString(),
                expires: new Date(session.expiresAt).toLocaleString(),
              })}
            >
              <div className="flex items-center justify-end gap-2">
                {session.current ? (
                  <Chip color="success" variant="tertiary">
                    {t("settings.sessions.currentChip")}
                  </Chip>
                ) : (
                  <Button
                    isIconOnly
                    size="sm"
                    variant="ghost"
                    aria-label={t("settings.sessions.revoke.aria")}
                    isDisabled={revoke.isPending && revokeSessionId === session.id}
                    onPress={() => setRevokeSessionId(session.id)}
                  >
                    <TrashIcon className="size-4" />
                  </Button>
                )}
              </div>
            </SettingsRow>
          ))
        ) : (
          <div className="px-5 py-8 text-sm text-muted">{t("settings.sessions.empty")}</div>
        )}
      </SettingsSection>
      <ConfirmDialog
        open={revokeSessionId !== null}
        onOpenChange={(open) => {
          if (!open && !revoke.isPending) setRevokeSessionId(null);
        }}
        title={t("settings.sessions.revoke.title")}
        message={t("settings.sessions.revoke.message")}
        confirmLabel={t("settings.sessions.revoke.confirm")}
        isPending={revoke.isPending}
        onConfirm={() => {
          if (revokeSessionId) {
            revoke.mutate({ params: { path: { sessionId: revokeSessionId } } });
          }
        }}
      />
    </div>
  );
}
