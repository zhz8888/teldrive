import { Button, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import type { ApiKeyCreated } from "@/api/types";
import { ConfirmDialog } from "@/components/dialogs/confirm-dialog";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { newIdempotencyKey } from "@/features/shared/idempotency";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import CopyIcon from "~icons/gravity-ui/copy";
import TrashIcon from "~icons/gravity-ui/trash-bin";

export const Route = createFileRoute("/_settings/settings/api-keys")({
  component: ApiKeysSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function ApiKeysSettings() {
  const { t } = useI18n();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<ApiKeyCreated>();
  const [revokeKey, setRevokeKey] = useState<{ id: string; name: string } | null>(null);
  const query = $api.useSuspenseQuery(
    "get",
    "/v1/api-keys",
    { params: { query: { limit: 200 } } },
    { staleTime: 20_000 },
  );
  const create = $api.useMutation("post", "/v1/api-keys");
  const revoke = $api.useMutation("delete", "/v1/api-keys/{apiKeyId}", {
    onSuccess: () => {
      setRevokeKey(null);
      void refresh();
      toast.success(t("settings.apiKeys.toast.revoked"));
    },
    onError: (error) => {
      toast.error(t("settings.apiKeys.toast.revokeFailed"), { description: userMessage(error) });
    },
  });
  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/api-keys").queryKey,
    });
  const formatDate = (value?: string | null) =>
    value ? new Date(value).toLocaleString() : t("settings.apiKeys.never");

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.apiKeys.title")}
        description={t("settings.apiKeys.description")}
      />
      <SettingsSection
        title={t("settings.apiKeys.create.section")}
        description={t("settings.apiKeys.create.description")}
      >
        <SettingsRow
          label={t("settings.apiKeys.name.label")}
          description={t("settings.apiKeys.name.description")}
        >
          <div className="flex gap-2">
            <TextField className="min-w-0 flex-1">
              <Label className="sr-only">{t("settings.apiKeys.name.label")}</Label>
              <Input
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
                placeholder={t("settings.apiKeys.name.placeholder")}
              />
            </TextField>
            <Button
              onPress={async () => {
                if (!name.trim()) return;
                try {
                  const result = await create.mutateAsync({
                    params: { header: { "Idempotency-Key": newIdempotencyKey() } },
                    body: { name: name.trim() },
                  });
                  setCreated(result);
                  setName("");
                  await refresh();
                } catch (error) {
                  toast.error(t("settings.apiKeys.toast.createFailed"), {
                    description: userMessage(error),
                  });
                }
              }}
              isDisabled={!name.trim() || create.isPending}
            >
              {t("common.action.create")}
            </Button>
          </div>
        </SettingsRow>
        {created ? (
          <SettingsRow
            label={t("settings.apiKeys.secret.label")}
            description={t("settings.apiKeys.secret.description")}
          >
            <div className="flex gap-2">
              <Input readOnly value={created.secret} className="min-w-0 flex-1 font-mono" />
              <Button
                isIconOnly
                variant="secondary"
                aria-label={t("settings.apiKeys.copy")}
                onPress={() => {
                  void navigator.clipboard.writeText(created.secret);
                  toast.success(t("settings.apiKeys.copied"));
                }}
              >
                <CopyIcon className="size-4" />
              </Button>
            </div>
          </SettingsRow>
        ) : null}
      </SettingsSection>
      <SettingsSection
        title={t("settings.apiKeys.existing.section")}
        description={t("settings.apiKeys.existing.description")}
      >
        {query.data.items.length ? (
          query.data.items.map((item) => (
            <SettingsRow
              key={item.id}
              label={item.name}
              description={t("settings.apiKeys.row.description", {
                created: formatDate(item.createdAt),
                lastUsed: formatDate(item.lastUsedAt),
              })}
            >
              <div className="flex justify-end">
                <Button
                  isIconOnly
                  size="sm"
                  variant="ghost"
                  aria-label={t("settings.apiKeys.revoke.aria", { name: item.name })}
                  isDisabled={revoke.isPending && revokeKey?.id === item.id}
                  onPress={() => setRevokeKey({ id: item.id, name: item.name })}
                >
                  <TrashIcon className="size-4" />
                </Button>
              </div>
            </SettingsRow>
          ))
        ) : (
          <div className="px-5 py-8 text-sm text-muted">{t("settings.apiKeys.empty")}</div>
        )}
      </SettingsSection>
      <ConfirmDialog
        open={revokeKey !== null}
        onOpenChange={(open) => {
          if (!open && !revoke.isPending) setRevokeKey(null);
        }}
        title={t("settings.apiKeys.delete.title")}
        message={t("settings.apiKeys.delete.message", { name: revokeKey?.name ?? "" })}
        confirmLabel={t("settings.apiKeys.delete.confirm")}
        isPending={revoke.isPending}
        onConfirm={() => {
          if (revokeKey) {
            revoke.mutate({ params: { path: { apiKeyId: revokeKey.id } } });
          }
        }}
      />
    </div>
  );
}
