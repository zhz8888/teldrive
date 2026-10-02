import { Button, Chip, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { ConfirmDialog } from "@/components/dialogs/confirm-dialog";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { newIdempotencyKey } from "@/features/shared/idempotency";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import RefreshIcon from "~icons/gravity-ui/arrow-rotate-left";
import CheckIcon from "~icons/gravity-ui/check";
import TrashIcon from "~icons/gravity-ui/trash-bin";

export const Route = createFileRoute("/_settings/settings/channels")({
  component: ChannelsSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function ChannelsSettings() {
  const { t } = useI18n();
  const [name, setName] = useState("");
  const [deleteChannel, setDeleteChannel] = useState<{ id: number; name: string } | null>(null);
  const query = $api.useSuspenseQuery(
    "get",
    "/v1/channels",
    { params: { query: { limit: 200 } } },
    { staleTime: 20_000 },
  );
  const create = $api.useMutation("post", "/v1/channels");
  const select = $api.useMutation("post", "/v1/channels/{channelId}/select");
  const sync = $api.useMutation("post", "/v1/channels/sync");
  const remove = $api.useMutation("delete", "/v1/channels/{channelId}", {
    onSuccess: () => {
      setDeleteChannel(null);
      void refresh();
      toast.success(t("settings.channels.toast.deleted"));
    },
    onError: (error) => {
      toast.error(t("settings.channels.toast.deleteFailed"), { description: userMessage(error) });
    },
  });
  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/channels").queryKey,
    });

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.channels.title")}
        description={t("settings.channels.description")}
        actions={
          <Button
            variant="secondary"
            onPress={async () => {
              try {
                await sync.mutateAsync({
                  params: { header: { "Idempotency-Key": newIdempotencyKey() } },
                });
                await refresh();
                toast.success(t("settings.channels.toast.synced"));
              } catch (error) {
                toast.error(t("settings.channels.toast.syncFailed"), {
                  description: userMessage(error),
                });
              }
            }}
            isDisabled={sync.isPending}
          >
            <RefreshIcon className="size-4" />
            {t("settings.channels.sync")}
          </Button>
        }
      />
      <SettingsSection
        title={t("settings.channels.create.section")}
        description={t("settings.channels.create.description")}
      >
        <SettingsRow
          label={t("settings.channels.name.label")}
          description={t("settings.channels.name.description")}
        >
          <div className="flex gap-2">
            <TextField className="min-w-0 flex-1">
              <Label className="sr-only">{t("settings.channels.name.label")}</Label>
              <Input
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
                placeholder={t("settings.channels.name.placeholder")}
              />
            </TextField>
            <Button
              onPress={async () => {
                if (!name.trim()) return;
                try {
                  await create.mutateAsync({
                    params: { header: { "Idempotency-Key": newIdempotencyKey() } },
                    body: { name: name.trim(), selected: false },
                  });
                  setName("");
                  await refresh();
                  toast.success(t("settings.channels.toast.created"));
                } catch (error) {
                  toast.error(t("settings.channels.toast.createFailed"), {
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
      </SettingsSection>
      <SettingsSection
        title={t("settings.channels.configured.section")}
        description={t("settings.channels.configured.description")}
      >
        {query.data.items.length ? (
          query.data.items.map((channel) => (
            <SettingsRow
              key={channel.id}
              label={channel.name}
              description={t("settings.channels.row.description", { id: channel.id })}
            >
              <div className="flex items-center justify-end gap-2">
                {channel.selected ? (
                  <Chip color="success" variant="tertiary">
                    <CheckIcon className="size-3" />
                    {t("settings.channels.selected")}
                  </Chip>
                ) : (
                  <Button
                    size="sm"
                    variant="secondary"
                    onPress={async () => {
                      await select.mutateAsync({ params: { path: { channelId: channel.id } } });
                      await refresh();
                    }}
                  >
                    {t("settings.channels.use")}
                  </Button>
                )}
                <Button
                  isIconOnly
                  size="sm"
                  variant="ghost"
                  aria-label={t("settings.channels.delete.aria", { name: channel.name })}
                  isDisabled={remove.isPending && deleteChannel?.id === channel.id}
                  onPress={() => setDeleteChannel({ id: channel.id, name: channel.name })}
                >
                  <TrashIcon className="size-4" />
                </Button>
              </div>
            </SettingsRow>
          ))
        ) : (
          <div className="px-5 py-8 text-sm text-muted">{t("settings.channels.empty")}</div>
        )}
      </SettingsSection>
      <ConfirmDialog
        open={deleteChannel !== null}
        onOpenChange={(open) => {
          if (!open && !remove.isPending) setDeleteChannel(null);
        }}
        title={t("settings.channels.delete.title")}
        message={t("settings.channels.delete.message", { name: deleteChannel?.name ?? "" })}
        confirmLabel={t("settings.channels.delete.confirm")}
        isPending={remove.isPending}
        onConfirm={() => {
          if (deleteChannel) {
            remove.mutate({ params: { path: { channelId: deleteChannel.id } } });
          }
        }}
      />
    </div>
  );
}
