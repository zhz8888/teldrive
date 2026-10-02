import { Button, Chip, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import RefreshIcon from "~icons/gravity-ui/arrow-rotate-left";

export const Route = createFileRoute("/_settings/settings/users")({
  component: UsersSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

function UsersSettings() {
  const { t } = useI18n();
  const [search, setSearch] = useState("");
  const query = $api.useSuspenseQuery(
    "get",
    "/v1/admin/users",
    { params: { query: { search: search.trim() || undefined } } },
    { staleTime: 10_000 },
  );
  const updateUser = $api.useMutation("patch", "/v1/admin/users/{userId}");
  const revokeAccess = $api.useMutation("post", "/v1/admin/users/{userId}/revoke-access");
  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/admin/users").queryKey,
    });

  const update = async (userId: number, body: { role?: "admin" | "user"; disabled?: boolean }) => {
    try {
      await updateUser.mutateAsync({ params: { path: { userId } }, body });
      await refresh();
      toast.success(t("settings.users.toast.updated"));
    } catch (error) {
      toast.error(t("settings.users.toast.updateFailed"), { description: userMessage(error) });
    }
  };

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.users.title")}
        description={t("settings.users.description")}
        actions={
          <Button variant="secondary" onPress={() => void refresh()}>
            <RefreshIcon className="size-4" />
            {t("common.action.refresh")}
          </Button>
        }
      />

      <SettingsSection
        title={t("settings.users.section")}
        description={t("settings.users.section.description")}
      >
        <div className="border-b border-border p-4">
          <TextField value={search} onChange={setSearch} className="max-w-md">
            <Label>{t("settings.users.search.label")}</Label>
            <Input placeholder={t("settings.users.search.placeholder")} />
          </TextField>
        </div>
        {query.data.length ? (
          query.data.map((user) => {
            const displayName =
              user.displayName?.trim() ||
              user.username?.trim() ||
              t("settings.users.displayNameFallback", { userId: user.userId });
            const owner = user.role === "owner";
            return (
              <SettingsRow
                key={user.userId}
                label={displayName}
                description={
                  user.username
                    ? t("settings.users.row.descriptionWithUsername", {
                        handle: `@${user.username}`,
                        id: user.userId,
                      })
                    : t("settings.users.row.description", { id: user.userId })
                }
              >
                <div className="flex flex-wrap items-center justify-end gap-2">
                  <Chip
                    variant="tertiary"
                    color={owner ? "accent" : user.role === "admin" ? "warning" : "default"}
                  >
                    {user.role}
                  </Chip>
                  {user.disabled ? (
                    <Chip variant="tertiary" color="danger">
                      {t("settings.users.disabled")}
                    </Chip>
                  ) : null}
                  {!owner ? (
                    <Button
                      size="sm"
                      variant="secondary"
                      isDisabled={updateUser.isPending}
                      onPress={() =>
                        void update(user.userId, { role: user.role === "admin" ? "user" : "admin" })
                      }
                    >
                      {user.role === "admin"
                        ? t("settings.users.makeUser")
                        : t("settings.users.makeAdmin")}
                    </Button>
                  ) : null}
                  {!owner ? (
                    <Button
                      size="sm"
                      variant={user.disabled ? "secondary" : "danger"}
                      isDisabled={updateUser.isPending}
                      onPress={() => void update(user.userId, { disabled: !user.disabled })}
                    >
                      {user.disabled ? t("settings.users.enable") : t("settings.users.disable")}
                    </Button>
                  ) : null}
                  {!owner ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      isDisabled={revokeAccess.isPending}
                      onPress={async () => {
                        try {
                          await revokeAccess.mutateAsync({
                            params: { path: { userId: user.userId } },
                          });
                          toast.success(t("settings.users.toast.revoked"));
                        } catch (error) {
                          toast.error(t("settings.users.toast.revokeFailed"), {
                            description: userMessage(error),
                          });
                        }
                      }}
                    >
                      {t("settings.users.revokeAccess")}
                    </Button>
                  ) : null}
                </div>
              </SettingsRow>
            );
          })
        ) : (
          <p className="p-6 text-sm text-muted">{t("settings.users.empty")}</p>
        )}
      </SettingsSection>
    </div>
  );
}
