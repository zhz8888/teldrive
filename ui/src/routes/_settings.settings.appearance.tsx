import { Button } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useTheme } from "next-themes";
import DisplayIcon from "~icons/gravity-ui/display";
import MoonIcon from "~icons/gravity-ui/moon";
import SunIcon from "~icons/gravity-ui/sun";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { LOCALE_LABELS, LOCALES, type MessageKey, useI18n } from "@/lib/i18n";

export const Route = createFileRoute("/_settings/settings/appearance")({
  component: AppearanceSettings,
});

// The colour-theme choices in the order they are shown. Following the system
// preference comes first because it is the mode a visitor starts in.
const THEME_OPTIONS = [
  { value: "system", icon: DisplayIcon, labelKey: "settings.appearance.colorTheme.system" },
  { value: "light", icon: SunIcon, labelKey: "settings.appearance.colorTheme.light" },
  { value: "dark", icon: MoonIcon, labelKey: "settings.appearance.colorTheme.dark" },
] as const satisfies readonly { value: string; icon: unknown; labelKey: MessageKey }[];

function AppearanceSettings() {
  const { theme, setTheme } = useTheme();
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
          <div className="flex flex-wrap gap-2">
            {THEME_OPTIONS.map((option) => (
              <Button
                key={option.value}
                className="min-w-28 flex-1"
                variant={theme === option.value ? "primary" : "secondary"}
                aria-pressed={theme === option.value}
                onPress={() => setTheme(option.value)}
              >
                <option.icon className="size-4" />
                {t(option.labelKey)}
              </Button>
            ))}
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
