import { Button, Modal, Separator, Typography } from "@heroui/react";
import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useLocation } from "@tanstack/react-router";
import { useState } from "react";
import { currentUserQueryOptions } from "@/auth/queries";
import { type MessageKey, useI18n } from "@/lib/i18n";
import UploadIcon from "~icons/gravity-ui/arrow-up-from-line";
import MenuIcon from "~icons/gravity-ui/bars";
import ClockIcon from "~icons/gravity-ui/clock";
import StorageIcon from "~icons/gravity-ui/database";
import KeyIcon from "~icons/gravity-ui/key";
import RobotIcon from "~icons/gravity-ui/layers";
import SessionsIcon from "~icons/gravity-ui/list-ul";
import PaletteIcon from "~icons/gravity-ui/palette";
import PersonIcon from "~icons/gravity-ui/person";
import CloseIcon from "~icons/gravity-ui/xmark";

const SETTINGS_GROUPS = [
  {
    labelKey: "settings.layout.group.account",
    items: [
      { labelKey: "settings.layout.nav.overview", path: "/settings", icon: PersonIcon },
      {
        labelKey: "settings.layout.nav.users",
        path: "/settings/users",
        icon: PersonIcon,
        capability: "system.manageUsers",
      },
    ],
  },
  {
    labelKey: "settings.layout.group.telegramStorage",
    items: [
      { labelKey: "settings.layout.nav.channels", path: "/settings/channels", icon: StorageIcon },
      { labelKey: "settings.layout.nav.bots", path: "/settings/bots", icon: RobotIcon },
    ],
  },
  {
    labelKey: "settings.layout.group.security",
    items: [
      { labelKey: "settings.layout.nav.sessions", path: "/settings/sessions", icon: SessionsIcon },
      { labelKey: "settings.layout.nav.apiKeys", path: "/settings/api-keys", icon: KeyIcon },
    ],
  },
  {
    labelKey: "settings.layout.group.preferences",
    items: [
      { labelKey: "settings.layout.nav.uploads", path: "/settings/uploads", icon: UploadIcon },
      {
        labelKey: "settings.layout.nav.appearance",
        path: "/settings/appearance",
        icon: PaletteIcon,
      },
    ],
  },
  {
    labelKey: "settings.layout.group.system",
    items: [
      {
        labelKey: "settings.layout.nav.periodicJobs",
        path: "/settings/periodic-jobs",
        icon: ClockIcon,
        capability: "system.maintenance",
      },
    ],
  },
] as const;

export const Route = createFileRoute("/_settings")({ component: SettingsLayout });

function SettingsLayout() {
  const { t } = useI18n();
  const location = useLocation();
  const { data: user } = useQuery(currentUserQueryOptions());
  const [mobileOpen, setMobileOpen] = useState(false);
  const visibleGroups = settingsGroupsFor(user?.capabilities ?? []);
  const activeLabelKey = visibleGroups.reduce<MessageKey | undefined>(
    (found, group) =>
      found ?? group.items.find((item) => item.path === location.pathname)?.labelKey,
    undefined,
  );

  return (
    <div className="mx-auto grid w-full max-w-7xl gap-6 lg:grid-cols-[15rem_minmax(0,1fr)]">
      <aside className="hidden lg:block">
        <div className="sticky top-0 flex max-h-[calc(100dvh-7rem)] flex-col gap-5 overflow-y-auto border-r border-border pr-5">
          <div>
            <Typography type="h2" className="text-lg font-semibold">
              {t("settings.layout.title")}
            </Typography>
            <Typography.Paragraph className="mt-1 text-xs text-muted">
              {t("settings.layout.description")}
            </Typography.Paragraph>
          </div>
          <SettingsNavigation currentPath={location.pathname} groups={visibleGroups} />
        </div>
      </aside>
      <main className="min-w-0">
        <div className="mb-5 flex items-center justify-between border-b border-border pb-4 lg:hidden">
          <div>
            <Typography type="h2" className="text-base font-semibold">
              {activeLabelKey ? t(activeLabelKey) : t("settings.layout.title")}
            </Typography>
            <Typography.Paragraph className="text-xs text-muted">
              {t("settings.layout.mobileSubtitle")}
            </Typography.Paragraph>
          </div>
          <Button
            isIconOnly
            size="sm"
            variant="tertiary"
            aria-label={t("settings.layout.openNavigation")}
            onPress={() => setMobileOpen(true)}
          >
            <MenuIcon className="size-4" />
          </Button>
        </div>
        <Outlet />
      </main>
      <Modal.Backdrop isOpen={mobileOpen} onOpenChange={setMobileOpen} isDismissable>
        <Modal.Container size="sm" className="mr-auto h-dvh max-h-dvh rounded-none">
          <Modal.Dialog className="h-full rounded-none">
            <Modal.Header className="flex-row items-center justify-between border-b border-border">
              <div>
                <Modal.Heading>{t("settings.layout.title")}</Modal.Heading>
                <Typography.Paragraph className="text-xs text-muted">
                  {t("settings.layout.chooseArea")}
                </Typography.Paragraph>
              </div>
              <Button
                isIconOnly
                size="sm"
                variant="tertiary"
                aria-label={t("settings.layout.closeNavigation")}
                onPress={() => setMobileOpen(false)}
              >
                <CloseIcon className="size-4" />
              </Button>
            </Modal.Header>
            <Modal.Body className="py-5">
              <SettingsNavigation
                currentPath={location.pathname}
                groups={visibleGroups}
                onNavigate={() => setMobileOpen(false)}
              />
            </Modal.Body>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </div>
  );
}

function SettingsNavigation({
  currentPath,
  groups,
  onNavigate,
}: {
  currentPath: string;
  groups: SettingsGroup[];
  onNavigate?: () => void;
}) {
  const { t } = useI18n();
  return (
    <nav aria-label={t("settings.layout.navigationLabel")} className="flex flex-col gap-5">
      {groups.map((group, groupIndex) => (
        <div key={group.labelKey} className="flex flex-col gap-1.5">
          {groupIndex > 0 ? <Separator className="mb-3" /> : null}
          <p className="px-2 text-[0.68rem] font-semibold uppercase tracking-[0.14em] text-muted">
            {t(group.labelKey)}
          </p>
          {group.items.map((item) => {
            const active = currentPath === item.path;
            return (
              <Link
                key={item.path}
                to={item.path}
                onClick={onNavigate}
                className={`flex min-h-10 items-center gap-3 rounded-lg px-3 text-sm font-medium transition-colors ${active ? "bg-accent/10 text-accent" : "text-muted hover:bg-default/30 hover:text-foreground"}`}
              >
                <item.icon className="size-4 shrink-0" />
                <span>{t(item.labelKey)}</span>
              </Link>
            );
          })}
        </div>
      ))}
    </nav>
  );
}

type SettingsItem = (typeof SETTINGS_GROUPS)[number]["items"][number];
type SettingsGroup = { labelKey: MessageKey; items: SettingsItem[] };

function settingsGroupsFor(capabilities: string[]): SettingsGroup[] {
  const allowed = new Set(capabilities);
  return SETTINGS_GROUPS.map((group) => ({
    labelKey: group.labelKey,
    items: group.items.filter((item) => !("capability" in item) || allowed.has(item.capability)),
  })).filter((group) => group.items.length > 0) as SettingsGroup[];
}
