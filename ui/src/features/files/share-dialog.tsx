import {
  Button,
  Chip,
  Dropdown,
  Input,
  Label,
  ListBox,
  Select,
  Spinner,
  TextField,
} from "@heroui/react";
import { useQuery } from "@tanstack/react-query";
import { useDeferredValue, useEffect, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import type { FileEntry } from "@/api/types";
import { AppDialog } from "@/components/dialogs/app-dialog";
import { copyText } from "@/features/files/download";
import { type MessageKey, useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";

/** Access level a grant or public link carries: read-only viewer or full editor. */
type Permission = "read" | "edit";
/** Expiry presets offered by {@link ExpirationPicker}; "custom" reveals a free-text field. */
type ExpirationMode = "never" | "1h" | "1d" | "7d" | "30d" | "custom";

// Milliseconds per unit suffix accepted in a custom duration. The units are case-sensitive:
// lowercase "m" is a minute while uppercase "M" is a 30-day month.
const DURATION_UNITS: Record<string, number> = {
  ms: 1,
  s: 1000,
  m: 60 * 1000,
  h: 60 * 60 * 1000,
  d: 24 * 60 * 60 * 1000,
  w: 7 * 24 * 60 * 60 * 1000,
  M: 30 * 24 * 60 * 60 * 1000,
  y: 365 * 24 * 60 * 60 * 1000,
};

/** Message key of the label the picker shows for each expiration mode. */
const EXPIRATION_LABELS = {
  never: "features.shareDialog.noExpiration",
  "1h": "features.shareDialog.duration.oneHour",
  "1d": "features.shareDialog.duration.oneDay",
  "7d": "features.shareDialog.duration.sevenDays",
  "30d": "features.shareDialog.duration.thirtyDays",
  custom: "features.shareDialog.duration.custom",
} as const satisfies Record<ExpirationMode, MessageKey>;

/**
 * Sharing dialog for one file: it grants access to individual users and creates or revokes
 * public links, each with its own permission and expiry. `file` also drives visibility —
 * an undefined file keeps the dialog closed — and its id resets every field, so switching
 * targets never carries the previous file's password or expiry over.
 */
export function ShareDialog({
  file,
  onOpenChange,
}: {
  file?: FileEntry;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useI18n();
  const fileId = file?.id ?? "00000000-0000-0000-0000-000000000000";
  const [peopleSearch, setPeopleSearch] = useState("");
  const deferredPeopleSearch = useDeferredValue(peopleSearch.trim());
  const [peoplePermission, setPeoplePermission] = useState<Permission>("read");
  const [linkPermission, setLinkPermission] = useState<Permission>("read");
  const [linkPassword, setLinkPassword] = useState("");
  const [peopleExpirationMode, setPeopleExpirationMode] = useState<ExpirationMode>("never");
  const [peopleDuration, setPeopleDuration] = useState("1d");
  const [linkExpirationMode, setLinkExpirationMode] = useState<ExpirationMode>("never");
  const [linkDuration, setLinkDuration] = useState("1d");
  const [createdUrl, setCreatedUrl] = useState("");

  useEffect(() => {
    setPeopleSearch("");
    setPeoplePermission("read");
    setLinkPermission("read");
    setLinkPassword("");
    setPeopleExpirationMode("never");
    setPeopleDuration("1d");
    setLinkExpirationMode("never");
    setLinkDuration("1d");
    setCreatedUrl("");
  }, [file?.id]);

  const grantsQuery = useQuery({
    ...$api.queryOptions("get", "/v1/files/{fileId}/grants", {
      params: { path: { fileId } },
    }),
    enabled: Boolean(file),
  });
  const linksQuery = useQuery({
    ...$api.queryOptions("get", "/v1/files/{fileId}/shares", {
      params: { path: { fileId }, query: { limit: 200 } },
    }),
    enabled: Boolean(file),
  });
  const usersQuery = useQuery({
    ...$api.queryOptions("get", "/v1/users/search", {
      params: { query: { search: deferredPeopleSearch || "_" } },
    }),
    enabled: Boolean(file) && deferredPeopleSearch.length >= 2,
  });

  const createGrant = $api.useMutation("post", "/v1/files/{fileId}/grants");
  const updateGrant = $api.useMutation("patch", "/v1/grants/{grantId}");
  const revokeGrant = $api.useMutation("delete", "/v1/grants/{grantId}");
  const createLink = $api.useMutation("post", "/v1/files/{fileId}/shares");
  const revokeLink = $api.useMutation("delete", "/v1/shares/{shareId}");

  const refreshGrants = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/files/{fileId}/grants", {
        params: { path: { fileId } },
      }).queryKey,
    });
  const refreshLinks = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/files/{fileId}/shares", {
        params: { path: { fileId } },
      }).queryKey,
    });

  const addPerson = async (userId: number) => {
    if (!file) return;
    try {
      await createGrant.mutateAsync({
        params: { path: { fileId: file.id } },
        body: {
          granteeUserId: userId,
          permission: peoplePermission,
          expiresAt: expirationDate(peopleExpirationMode, peopleDuration),
        },
      });
      setPeopleSearch("");
      await refreshGrants();
      toast.success(t("features.shareDialog.toast.accessGranted"));
    } catch (error) {
      toast.error(t("features.shareDialog.toast.accessGrantFailed"), {
        description: userMessage(error),
      });
    }
  };

  const createPublicLink = async () => {
    if (!file) return;
    try {
      const result = await createLink.mutateAsync({
        params: {
          path: { fileId: file.id },
        },
        body: {
          password: linkPassword.trim() || undefined,
          permission: linkPermission,
          expiresAt: expirationDate(linkExpirationMode, linkDuration),
        },
      });
      const publicUrl = new URL(result.publicUrl, window.location.origin).toString();
      setCreatedUrl(publicUrl);
      await copyText(publicUrl);
      await refreshLinks();
      toast.success(t("features.shareDialog.toast.publicLinkCreated"));
    } catch (error) {
      toast.error(t("features.shareDialog.toast.publicLinkCreateFailed"), {
        description: userMessage(error),
      });
    }
  };

  return (
    <AppDialog
      open={Boolean(file)}
      onOpenChange={onOpenChange}
      title={
        file
          ? t("features.shareDialog.title", { name: file.name })
          : t("features.shareDialog.titleFallback")
      }
      description={t("features.shareDialog.description")}
      size="lg"
      className="sm:w-[min(92vw,46rem)]"
      bodyClassName="py-4"
      footer={
        <Button variant="secondary" onPress={() => onOpenChange(false)}>
          {t("common.action.close")}
        </Button>
      }
    >
      <div className="grid gap-5">
        <section className="grid gap-4 rounded-2xl border border-border bg-default/10 p-4">
          <div>
            <h3 className="text-sm font-semibold">{t("features.shareDialog.peopleWithAccess")}</h3>
            <p className="mt-1 text-xs text-muted">{t("features.shareDialog.accessScopeHint")}</p>
          </div>

          <TextField value={peopleSearch} onChange={setPeopleSearch}>
            <Label>{t("features.shareDialog.addUser")}</Label>
            <Input placeholder={t("features.shareDialog.searchUsers")} />
          </TextField>

          <div className="grid gap-3 sm:grid-cols-2">
            <ControlField label={t("features.shareDialog.permission")}>
              <PermissionPicker value={peoplePermission} onChange={setPeoplePermission} />
            </ControlField>
            <ControlField label={t("features.shareDialog.expiration")}>
              <ExpirationPicker
                mode={peopleExpirationMode}
                duration={peopleDuration}
                onModeChange={setPeopleExpirationMode}
                onDurationChange={setPeopleDuration}
              />
            </ControlField>
          </div>

          {deferredPeopleSearch.length >= 2 ? (
            usersQuery.isPending ? (
              <div className="flex justify-center py-3">
                <Spinner size="sm" />
              </div>
            ) : usersQuery.data?.length ? (
              <div className="grid gap-1 rounded-xl border border-border bg-surface p-1">
                {usersQuery.data.map((user) => (
                  <Button
                    key={user.userId}
                    variant="ghost"
                    className="h-auto justify-start px-3 py-2 text-left"
                    isDisabled={createGrant.isPending}
                    onPress={() => void addPerson(user.userId)}
                  >
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium">
                        {user.displayName?.trim() ||
                          user.username?.trim() ||
                          t("features.shareDialog.userFallback", { id: user.userId })}
                      </span>
                      <span className="block truncate text-xs text-muted">
                        {user.username ? `@${user.username} · ` : ""}
                        {t("features.shareDialog.telegramId", { id: user.userId })}
                      </span>
                    </span>
                  </Button>
                ))}
              </div>
            ) : (
              <p className="text-xs text-muted">{t("features.shareDialog.noUsers")}</p>
            )
          ) : null}

          <div className="grid min-h-12 content-start gap-2">
            {grantsQuery.isPending ? (
              <div className="flex justify-center py-3">
                <Spinner size="sm" />
              </div>
            ) : grantsQuery.data?.length ? (
              grantsQuery.data.map((grant) => (
                <div
                  key={grant.id}
                  className="flex items-center gap-3 rounded-xl border border-border bg-surface px-3 py-2.5"
                >
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium">
                      {grant.granteeDisplayName?.trim() ||
                        grant.granteeUsername?.trim() ||
                        t("features.shareDialog.userFallback", { id: grant.granteeUserId })}
                    </p>
                    <p className="truncate text-xs text-muted">
                      {grant.granteeUsername ? `@${grant.granteeUsername} · ` : ""}
                      {t("features.shareDialog.telegramId", { id: grant.granteeUserId })}
                    </p>
                    <p className="mt-0.5 truncate text-xs text-muted">
                      {grant.expiresAt
                        ? t("features.shareDialog.expires", {
                            date: new Date(grant.expiresAt).toLocaleString(),
                          })
                        : t("features.shareDialog.noExpiration")}
                    </p>
                  </div>
                  <Chip variant="tertiary">
                    {t(
                      grant.permission === "read"
                        ? "features.shareDialog.viewer"
                        : "features.shareDialog.editor",
                    )}
                  </Chip>
                  <Dropdown>
                    <Button
                      isIconOnly
                      size="sm"
                      variant="ghost"
                      aria-label={t("features.shareDialog.grantActions")}
                    >
                      <span className="text-lg leading-none">⋯</span>
                    </Button>
                    <Dropdown.Popover className="min-w-48">
                      <Dropdown.Menu
                        aria-label={t("features.shareDialog.grantActions")}
                        onAction={(key) => {
                          if (key === "permission") {
                            void (async () => {
                              try {
                                await updateGrant.mutateAsync({
                                  params: { path: { grantId: grant.id } },
                                  body: {
                                    permission: grant.permission === "read" ? "edit" : "read",
                                    clearExpiresAt: false,
                                  },
                                });
                                await refreshGrants();
                              } catch (error) {
                                toast.error(
                                  t("features.shareDialog.toast.permissionChangeFailed"),
                                  {
                                    description: userMessage(error),
                                  },
                                );
                              }
                            })();
                          }
                          if (key === "remove") {
                            void (async () => {
                              try {
                                await revokeGrant.mutateAsync({
                                  params: { path: { grantId: grant.id } },
                                });
                                await refreshGrants();
                                toast.success(t("features.shareDialog.toast.accessRemoved"));
                              } catch (error) {
                                toast.error(t("features.shareDialog.toast.accessRemoveFailed"), {
                                  description: userMessage(error),
                                });
                              }
                            })();
                          }
                        }}
                      >
                        <Dropdown.Item
                          id="permission"
                          textValue={t("features.shareDialog.changePermission")}
                        >
                          <Label>
                            {t(
                              grant.permission === "read"
                                ? "features.shareDialog.makeEditor"
                                : "features.shareDialog.makeViewer",
                            )}
                          </Label>
                        </Dropdown.Item>
                        <Dropdown.Item
                          id="remove"
                          textValue={t("features.shareDialog.removeAccess")}
                        >
                          <Label>{t("features.shareDialog.removeAccess")}</Label>
                        </Dropdown.Item>
                      </Dropdown.Menu>
                    </Dropdown.Popover>
                  </Dropdown>
                </div>
              ))
            ) : (
              <p className="flex min-h-12 items-center text-xs text-muted">
                {t("features.shareDialog.noGrants")}
              </p>
            )}
          </div>
        </section>

        <section className="grid gap-4 rounded-2xl border border-border bg-default/10 p-4">
          <div>
            <h3 className="text-sm font-semibold">{t("features.shareDialog.publicLinks")}</h3>
            <p className="mt-1 text-xs text-muted">{t("features.shareDialog.publicLinksHint")}</p>
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <ControlField label={t("features.shareDialog.permission")}>
              <PermissionPicker value={linkPermission} onChange={setLinkPermission} />
            </ControlField>
            <ControlField label={t("features.shareDialog.expiration")}>
              <ExpirationPicker
                mode={linkExpirationMode}
                duration={linkDuration}
                onModeChange={setLinkExpirationMode}
                onDurationChange={setLinkDuration}
              />
            </ControlField>
          </div>

          <TextField value={linkPassword} onChange={setLinkPassword}>
            <Label>{t("common.label.password")}</Label>
            <Input type="password" placeholder={t("features.shareDialog.optionalPassword")} />
          </TextField>

          <div>
            <Button
              variant="primary"
              isDisabled={createLink.isPending}
              onPress={() => void createPublicLink()}
            >
              {t("features.shareDialog.createPublicLink")}
            </Button>
          </div>

          {createdUrl ? (
            <div className="flex gap-2 rounded-xl border border-border bg-surface p-3">
              <Input readOnly value={createdUrl} className="min-w-0 flex-1" />
              <Button
                size="sm"
                variant="secondary"
                onPress={async () => {
                  try {
                    await copyText(createdUrl);
                    toast.success(t("features.shareDialog.linkCopied"));
                  } catch (error) {
                    toast.error(t("features.shareDialog.linkCopyFailed"), {
                      description: userMessage(error),
                    });
                  }
                }}
              >
                {t("common.action.copy")}
              </Button>
            </div>
          ) : null}

          <div className="grid min-h-12 content-start gap-2">
            {linksQuery.isPending ? (
              <div className="flex justify-center py-3">
                <Spinner size="sm" />
              </div>
            ) : linksQuery.data?.items.length ? (
              linksQuery.data.items.map((link) => (
                <div
                  key={link.id}
                  className="flex items-center gap-3 rounded-xl border border-border bg-surface px-3 py-2.5"
                >
                  <div className="min-w-0 flex-1">
                    <p className="text-sm font-medium">
                      {t(
                        link.permission === "read"
                          ? "features.shareDialog.publicViewerLink"
                          : "features.shareDialog.publicEditorLink",
                      )}
                    </p>
                    <p className="truncate text-xs text-muted">
                      {[
                        link.passwordProtected
                          ? t("features.shareDialog.passwordProtected")
                          : undefined,
                        t("features.shareDialog.downloadCount", { count: link.downloadCount }),
                        link.expiresAt
                          ? t("features.shareDialog.expires", {
                              date: new Date(link.expiresAt).toLocaleString(),
                            })
                          : t("features.shareDialog.noExpiration"),
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </p>
                  </div>
                  <Chip variant="tertiary">
                    {t(
                      link.permission === "read"
                        ? "features.shareDialog.viewer"
                        : "features.shareDialog.editor",
                    )}
                  </Chip>
                  <Dropdown>
                    <Button
                      isIconOnly
                      size="sm"
                      variant="ghost"
                      aria-label={t("features.shareDialog.publicLinkActions")}
                    >
                      <span className="text-lg leading-none">⋯</span>
                    </Button>
                    <Dropdown.Popover className="min-w-40">
                      <Dropdown.Menu
                        aria-label={t("features.shareDialog.publicLinkActions")}
                        onAction={(key) => {
                          if (key !== "revoke") return;
                          void (async () => {
                            try {
                              await revokeLink.mutateAsync({
                                params: { path: { shareId: link.id } },
                              });
                              await refreshLinks();
                              toast.success(t("features.shareDialog.toast.publicLinkRevoked"));
                            } catch (error) {
                              toast.error(t("features.shareDialog.toast.publicLinkRevokeFailed"), {
                                description: userMessage(error),
                              });
                            }
                          })();
                        }}
                      >
                        <Dropdown.Item id="revoke" textValue={t("features.shareDialog.revokeLink")}>
                          <Label>{t("features.shareDialog.revokeLink")}</Label>
                        </Dropdown.Item>
                      </Dropdown.Menu>
                    </Dropdown.Popover>
                  </Dropdown>
                </div>
              ))
            ) : (
              <p className="flex min-h-12 items-center text-xs text-muted">
                {t("features.shareDialog.noPublicLinks")}
              </p>
            )}
          </div>
        </section>
      </div>
    </AppDialog>
  );
}

/** Small muted label above one of the dialog's pickers. */
function ControlField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <span className="text-xs font-medium text-muted">{label}</span>
      {children}
    </div>
  );
}

/** Viewer/editor select shared by the grant and public-link sections. */
function PermissionPicker({
  value,
  onChange,
}: {
  value: Permission;
  onChange: (value: Permission) => void;
}) {
  const { t } = useI18n();
  return (
    <Select
      aria-label={t("features.shareDialog.sharePermission")}
      className="w-full"
      selectedKey={value}
      onSelectionChange={(key) => onChange(String(key) as Permission)}
    >
      <Select.Trigger className="h-9 min-h-9 py-1.5">
        <Select.Value>
          {t(value === "read" ? "features.shareDialog.viewer" : "features.shareDialog.editor")}
        </Select.Value>
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          <ListBox.Item id="read" textValue={t("features.shareDialog.viewer")}>
            {t("features.shareDialog.viewer")}
          </ListBox.Item>
          <ListBox.Item id="edit" textValue={t("features.shareDialog.editor")}>
            {t("features.shareDialog.editor")}
          </ListBox.Item>
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/**
 * Expiry select plus the free-text duration field, which is rendered only for the "custom"
 * mode. The duration string is kept by the parent so it survives switching modes; it is
 * not validated here but when the share is created.
 */
function ExpirationPicker({
  mode,
  duration,
  onModeChange,
  onDurationChange,
}: {
  mode: ExpirationMode;
  duration: string;
  onModeChange: (value: ExpirationMode) => void;
  onDurationChange: (value: string) => void;
}) {
  const { t } = useI18n();
  return (
    <div className="grid gap-2">
      <Select
        aria-label={t("features.shareDialog.expiration")}
        className="w-full"
        selectedKey={mode}
        onSelectionChange={(key) => onModeChange(String(key) as ExpirationMode)}
      >
        <Select.Trigger className="h-9 min-h-9 py-1.5">
          <Select.Value>{t(EXPIRATION_LABELS[mode])}</Select.Value>
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            <ListBox.Item id="never" textValue={t("features.shareDialog.noExpiration")}>
              {t("features.shareDialog.noExpiration")}
            </ListBox.Item>
            <ListBox.Item id="1h" textValue={t("features.shareDialog.duration.oneHour")}>
              {t("features.shareDialog.duration.oneHour")}
            </ListBox.Item>
            <ListBox.Item id="1d" textValue={t("features.shareDialog.duration.oneDay")}>
              {t("features.shareDialog.duration.oneDay")}
            </ListBox.Item>
            <ListBox.Item id="7d" textValue={t("features.shareDialog.duration.sevenDays")}>
              {t("features.shareDialog.duration.sevenDays")}
            </ListBox.Item>
            <ListBox.Item id="30d" textValue={t("features.shareDialog.duration.thirtyDays")}>
              {t("features.shareDialog.duration.thirtyDays")}
            </ListBox.Item>
            <ListBox.Item id="custom" textValue={t("features.shareDialog.duration.custom")}>
              {t("features.shareDialog.duration.customOption")}
            </ListBox.Item>
          </ListBox>
        </Select.Popover>
      </Select>
      {mode === "custom" ? (
        <TextField value={duration} onChange={onDurationChange}>
          <Input
            aria-label={t("features.shareDialog.customExpiration")}
            placeholder={t("features.shareDialog.customExpirationPlaceholder")}
          />
        </TextField>
      ) : null}
    </div>
  );
}

/**
 * Turns the picker state into the `expiresAt` the API expects: undefined for "never",
 * otherwise the current time plus the preset or custom duration, as an ISO instant. Throws
 * when the custom duration is unparsable or the resulting date overflows, which the calling
 * action reports as a failed share rather than silently creating a share without expiry.
 */
function expirationDate(mode: ExpirationMode, duration: string): string | undefined {
  if (mode === "never") return undefined;
  const milliseconds = parseDuration(mode === "custom" ? duration : mode);
  const expiresAt = new Date(Date.now() + milliseconds);
  if (!Number.isFinite(expiresAt.getTime())) throw new Error("Expiration duration is too large");
  return expiresAt.toISOString();
}

/**
 * Parses a duration such as "1h", "1d" or "1h30m" into milliseconds. The value must be
 * nothing but number/unit pairs, so "1 h" or a trailing separator is rejected; a zero or
 * overflowing total is rejected too. The thrown messages are shown to the user verbatim,
 * because `userMessage` passes a plain `Error` through unchanged.
 */
function parseDuration(input: string): number {
  const value = input.trim();
  if (!value) throw new Error("Enter an expiration duration such as 1h, 1d, or 1y");

  const pattern = /(\d+(?:\.\d+)?)(ms|s|m|h|d|w|M|y)/g;
  let total = 0;
  let offset = 0;
  for (const match of value.matchAll(pattern)) {
    if (match.index !== offset) throw new Error("Invalid expiration duration");
    total += Number(match[1]) * DURATION_UNITS[match[2]];
    offset = match.index + match[0].length;
  }
  if (offset !== value.length || !Number.isFinite(total) || total <= 0) {
    throw new Error("Invalid expiration duration. Try 1h, 1d, 1w, or 1y");
  }
  return total;
}
