import { Button, Chip, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import { useDebouncedValue } from "@/lib/use-debounced-value";
import RefreshIcon from "~icons/gravity-ui/arrow-rotate-left";

/** `/settings/users` — owner/admin view of every account, with role and access controls. */
export const Route = createFileRoute("/_settings/settings/users")({
  component: UsersSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

/**
 * Searchable account list for administrators: role changes, disable/enable and access
 * revocation, each followed by a listing refresh because the server is the authority on
 * what a change did to the row.
 */
function UsersSettings() {
  const { t } = useI18n();
  const [search, setSearch] = useState("");
  // The listing is fetched without suspense so typing never swaps the page for
  // the route pending component (which would drop the focus of the search
  // field); the debounce keeps the request rate down while typing.
  const debouncedSearch = useDebouncedValue(search.trim(), 300);
  const query = $api.useQuery(
    "get",
    "/v1/admin/users",
    { params: { query: { search: debouncedSearch || undefined } } },
    { staleTime: 10_000, placeholderData: (previous) => previous },
  );
  const updateUser = $api.useMutation("patch", "/v1/admin/users/{userId}");
  const revokeAccess = $api.useMutation("post", "/v1/admin/users/{userId}/revoke-access");
  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/admin/users").queryKey,
    });

  /** Applies one partial change to one account and reports the outcome as a toast. */
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
        {query.isPending ? (
          <div
            className="flex justify-center py-16"
            role="status"
            aria-label={t("settings.users.loading")}
          >
            <Spinner size="lg" />
          </div>
        ) : query.data?.length ? (
          query.data.map((user) => {
            const displayName =
              user.displayName?.trim() ||
              user.username?.trim() ||
              t("settings.users.displayNameFallback", { userId: user.userId });
            // The owner's role and access are not editable from this screen, so that row
            // renders its chips without any of the controls below.
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
