import { Button } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useTheme } from "next-themes";
import MoonIcon from "~icons/gravity-ui/moon";
import SunIcon from "~icons/gravity-ui/sun";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { LOCALE_LABELS, LOCALES, useI18n } from "@/lib/i18n";

export const Route = createFileRoute("/_settings/settings/appearance")({
  component: AppearanceSettings,
});

function AppearanceSettings() {
  const { resolvedTheme, setTheme } = useTheme();
  const { locale, setLocale, t } = useI18n();
  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.appearance.title")}
        description={t("settings.appearance.description")}
      />
      <SettingsSection
        title={t("settings.appearance.colorTheme.section")}
        description={t("settings.appearance.colorTheme.description")}
      >
        <SettingsRow
          label={t("settings.appearance.colorTheme.label")}
          description={t("settings.appearance.colorTheme.rowDescription")}
        >
          <div className="grid grid-cols-2 gap-2">
            <Button
              variant={resolvedTheme === "light" ? "primary" : "secondary"}
              onPress={() => setTheme("light")}
            >
              <SunIcon className="size-4" />
              {t("settings.appearance.colorTheme.light")}
            </Button>
            <Button
              variant={resolvedTheme === "dark" ? "primary" : "secondary"}
              onPress={() => setTheme("dark")}
            >
              <MoonIcon className="size-4" />
              {t("settings.appearance.colorTheme.dark")}
            </Button>
          </div>
        </SettingsRow>
      </SettingsSection>
      <SettingsSection
        title={t("settings.appearance.language.section")}
        description={t("settings.appearance.language.description")}
      >
        <SettingsRow
          label={t("settings.appearance.language.label")}
          description={t("settings.appearance.language.rowDescription")}
        >
          <div className="grid grid-cols-2 gap-2">
            {LOCALES.map((option) => (
              <Button
                key={option}
                variant={locale === option ? "primary" : "secondary"}
                aria-pressed={locale === option}
                onPress={() => setLocale(option)}
              >
                {LOCALE_LABELS[option]}
              </Button>
            ))}
          </div>
        </SettingsRow>
      </SettingsSection>
    </div>
  );
}
