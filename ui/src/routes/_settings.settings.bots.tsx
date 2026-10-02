import { Button, Chip, Label, Spinner, TextArea, TextField } from "@heroui/react";
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

export const Route = createFileRoute("/_settings/settings/bots")({
  component: BotsSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function BotsSettings() {
  const { t } = useI18n();
  const [token, setToken] = useState("");
  const [isAddingBots, setIsAddingBots] = useState(false);
  const [deleteBot, setDeleteBot] = useState<{ id: number; name: string } | null>(null);
  const query = $api.useSuspenseQuery(
    "get",
    "/v1/bots",
    { params: { query: { limit: 200 } } },
    { staleTime: 20_000 },
  );
  const create = $api.useMutation("post", "/v1/bots");
  const remove = $api.useMutation("delete", "/v1/bots/{botId}", {
    onSuccess: () => {
      setDeleteBot(null);
      void refresh();
      toast.success(t("settings.bots.toast.deleted"));
    },
    onError: (error) => {
      toast.error(t("settings.bots.toast.deleteFailed"), { description: userMessage(error) });
    },
  });
  const refresh = () =>
    getQueryClient().invalidateQueries({ queryKey: $api.queryOptions("get", "/v1/bots").queryKey });

  const handleAddBots = async () => {
    const tokens = [
      ...new Set(
        token
          .split(/\r?\n/)
          .map((value) => value.trim())
          .filter(Boolean),
      ),
    ];
    if (tokens.length === 0 || isAddingBots) return;
    setIsAddingBots(true);

    try {
      const result = await create.mutateAsync({
        body: { tokens },
      });
      const failed = new Set(result.failedIndexes);
      setToken(tokens.filter((_, index) => failed.has(index)).join("\n"));
      await refresh();
      if (result.bots.length > 0) {
        toast.success(t("settings.bots.queued", { count: result.bots.length }));
      }
      if (result.failedIndexes.length > 0) {
        toast.warning(t("settings.bots.invalidTokens", { count: result.failedIndexes.length }));
      }
    } catch (error) {
      toast.error(t("settings.bots.toast.queueFailed"), { description: userMessage(error) });
    } finally {
      setIsAddingBots(false);
    }
  };

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.bots.title")}
        description={t("settings.bots.description")}
      />
      <SettingsSection
        title={t("settings.bots.add.section")}
        description={t("settings.bots.add.description")}
      >
        <SettingsRow
          label={t("settings.bots.tokens.label")}
          description={t("settings.bots.tokens.description")}
        >
          <div className="flex flex-col items-stretch gap-2 sm:flex-row sm:items-end">
            <TextField className="min-w-0 flex-1">
              <Label className="sr-only">{t("settings.bots.tokens.label")}</Label>
              <TextArea
                value={token}
                onChange={(event) => setToken(event.currentTarget.value)}
                placeholder={"123456:ABC...\n789012:DEF..."}
                rows={5}
                className="h-32 min-h-32 max-h-32 resize-none overflow-y-auto font-mono"
              />
            </TextField>
            <Button
              className="shrink-0"
              onPress={handleAddBots}
              isDisabled={!token.trim() || isAddingBots}
              isPending={isAddingBots}
            >
              {t("settings.bots.add.action")}
            </Button>
          </div>
        </SettingsRow>
      </SettingsSection>
      <SettingsSection
        title={t("settings.bots.configured.section")}
        description={t("settings.bots.configured.description")}
      >
        {query.data.items.length ? (
          query.data.items.map((bot) => (
            <SettingsRow
              key={bot.id}
              label={`@${bot.username || `bot-${bot.id}`}`}
              description={t("settings.bots.added", {
                date: new Date(bot.createdAt).toLocaleString(),
              })}
            >
              <div className="flex items-center justify-end gap-2">
                <Chip color={bot.enabled ? "success" : "warning"} variant="tertiary">
                  {bot.enabled ? t("settings.bots.enabled") : t("settings.bots.disabled")}
                </Chip>
                <Button
                  isIconOnly
                  size="sm"
                  variant="ghost"
                  aria-label={t("settings.bots.delete.aria", {
                    name: bot.username || bot.id,
                  })}
                  isDisabled={remove.isPending && deleteBot?.id === bot.id}
                  onPress={() =>
                    setDeleteBot({ id: bot.id, name: bot.username || `bot-${bot.id}` })
                  }
                >
                  <TrashIcon className="size-4" />
                </Button>
              </div>
            </SettingsRow>
          ))
        ) : (
          <div className="px-5 py-8 text-sm text-muted">{t("settings.bots.empty")}</div>
        )}
      </SettingsSection>
      <ConfirmDialog
        open={deleteBot !== null}
        onOpenChange={(open) => {
          if (!open && !remove.isPending) setDeleteBot(null);
        }}
        title={t("settings.bots.delete.title")}
        message={t("settings.bots.delete.message", { name: deleteBot?.name ?? "" })}
        confirmLabel={t("settings.bots.delete.confirm")}
        isPending={remove.isPending}
        onConfirm={() => {
          if (deleteBot) {
            remove.mutate({ params: { path: { botId: deleteBot.id } } });
          }
        }}
      />
    </div>
  );
}
