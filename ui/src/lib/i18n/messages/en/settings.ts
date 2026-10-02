/**
 * English entries of the settings surfaces.
 *
 * Keys are `settings.<page>.<name>`; the page segment matches the route file
 * (`_settings.settings.<page>.tsx`) so a key is easy to locate from either side.
 */
export const settings = {
  "settings.appearance.title": "Appearance",
  "settings.appearance.description": "Control how Teldrive looks in this browser.",
  "settings.appearance.colorTheme.section": "Color theme",
  "settings.appearance.colorTheme.description":
    "The choice is stored locally and applies immediately.",
  "settings.appearance.colorTheme.label": "Theme",
  "settings.appearance.colorTheme.rowDescription":
    "Choose the light or dark Teldrive visual system.",
  "settings.appearance.colorTheme.light": "Light",
  "settings.appearance.colorTheme.dark": "Dark",
  "settings.appearance.language.section": "Language",
  "settings.appearance.language.description":
    "The choice is stored locally and applies immediately.",
  "settings.appearance.language.label": "Interface language",
  "settings.appearance.language.rowDescription":
    "Choose the language of menus, labels and messages. Log output stays in English.",
} as const;
