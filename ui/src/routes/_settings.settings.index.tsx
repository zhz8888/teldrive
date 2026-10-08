import { Button, Chip, Spinner } from "@heroui/react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { useCurrentUser } from "@/auth/use-current-user";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";
import LogoutIcon from "~icons/gravity-ui/arrow-right-from-square";

/** `/settings` — the account overview: identity, role, and drive-wide counters. */
export const Route = createFileRoute("/_settings/settings/")({
  component: AccountSettings,
  pendingComponent: () => (
    <div className="flex justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
});

/**
 * Formats a byte count with binary units. A non-finite or non-positive input reads as
 * "0 B" rather than "NaN B", and the unit is capped at PB so the exponent stays in range.
 */
function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const power = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** power).toFixed(power === 0 ? 0 : 1)} ${units[power]}`;
}

/**
 * Renders the signed-in user's profile and drive totals. The statistics are suspenseful,
 * so the route's `pendingComponent` covers the first load, and the 30-second stale time
 * keeps a revisit inside that window from showing the spinner again.
 */
function AccountSettings() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const user = useCurrentUser();
  const stats = $api.useSuspenseQuery(
    "get",
    "/v1/files/statistics/drive",
    {},
    { staleTime: 30_000 },
  );
  const logout = $api.useMutation("post", "/v1/auth/cookie/logout");

  // Like the sidebar's sign-out: the cache survives the session, so it is emptied before
  // the login screen can render it for the next user.
  const logOut = async () => {
    try {
      await logout.mutateAsync({});
      getQueryClient().clear();
      await navigate({ to: "/login", search: { redirect: "/files" }, replace: true });
    } catch (error) {
      toast.error(t("settings.account.logOutFailed"), { description: userMessage(error) });
    }
  };

  const displayName =
    user.data.displayName ||
    user.data.username ||
    t("settings.account.displayNameFallback", { userId: user.data.userId });

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.account.title")}
        description={t("settings.account.description")}
        actions={
          <Button variant="danger" onPress={logOut} isDisabled={logout.isPending}>
            <LogoutIcon className="size-4" />
            {t("settings.account.logOut")}
          </Button>
        }
      />
      <SettingsSection
        title={t("settings.account.profile.section")}
        description={t("settings.account.profile.description")}
      >
        <SettingsRow
          label={displayName}
          description={
            user.data.username
              ? `@${user.data.username}`
              : t("settings.account.telegramUser", { userId: user.data.userId })
          }
        >
          <div className="flex justify-end">
            <Chip
              variant="tertiary"
              color={
                user.data.role === "owner"
                  ? "accent"
                  : user.data.role === "admin"
                    ? "warning"
                    : "default"
              }
            >
              {t(
                user.data.role === "owner"
                  ? "settings.account.role.owner"
                  : user.data.role === "admin"
                    ? "settings.account.role.admin"
                    : "settings.account.role.user",
              )}
            </Chip>
          </div>
        </SettingsRow>
        <SettingsRow
          label={t("settings.account.created.label")}
          description={t("settings.account.created.description")}
        >
          <p className="text-right text-sm text-muted">
            {new Date(user.data.createdAt).toLocaleString()}
          </p>
        </SettingsRow>
      </SettingsSection>
      <SettingsSection
        title={t("settings.account.stats.section")}
        description={t("settings.account.stats.description")}
      >
        <SettingsRow label={t("settings.account.stats.files")}>
          <p className="text-right font-mono text-sm">{stats.data.totalFiles.toLocaleString()}</p>
        </SettingsRow>
        <SettingsRow label={t("settings.account.stats.storedData")}>
          <p className="text-right font-mono text-sm">{formatBytes(stats.data.totalBytes)}</p>
        </SettingsRow>
        <SettingsRow label={t("settings.account.stats.openUploads")}>
          <p className="text-right font-mono text-sm">{stats.data.openUploads.toLocaleString()}</p>
        </SettingsRow>
      </SettingsSection>
    </div>
  );
}
