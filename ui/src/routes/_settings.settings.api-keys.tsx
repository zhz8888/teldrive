import { Button, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import type { ApiKeyCreated } from "@/api/types";
import { ConfirmDialog } from "@/components/dialogs/confirm-dialog";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import CopyIcon from "~icons/gravity-ui/copy";
import TrashIcon from "~icons/gravity-ui/trash-bin";

/** `/settings/api-keys` — create and revoke the account's programmatic API keys. */
export const Route = createFileRoute("/_settings/settings/api-keys")({
  component: ApiKeysSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

/**
 * Lists the account's API keys and creates new ones. The secret of a freshly created key
 * is held in `created` because the API returns it only in the create response; the
 * listing that follows shows the key without it.
 */
function ApiKeysSettings() {
  const { t } = useI18n();
  const [name, setName] = useState("");
  // Secret of the key created in this visit; undefined hides the reveal row.
  const [created, setCreated] = useState<ApiKeyCreated>();
  // Key the confirmation dialog is asking about; null keeps it closed.
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
  /** Renders a timestamp, or the "never" label for a key that has no such value yet. */
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
                  // The clipboard can refuse (insecure origin, denied permission),
                  // so the confirmation waits for the write instead of claiming a
                  // copy that did not happen.
                  void navigator.clipboard.writeText(created.secret).then(
                    () => toast.success(t("settings.apiKeys.copied")),
                    () => toast.error(t("settings.apiKeys.copyFailed")),
                  );
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
          // Dismissal is ignored while the revoke is in flight, so the dialog cannot
          // close before the request that it started has settled.
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
