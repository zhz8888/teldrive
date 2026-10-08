import { Label, ListBox, NumberField, Select, Switch } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useRef, useState } from "react";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { MAX_PART_SIZE_MIB, normalizePartSizeMiB, useUploadStore } from "@/features/uploads/store";
import { useI18n } from "@/lib/i18n";

/** `/settings/uploads` — browser-local defaults for new upload batches. */
export const Route = createFileRoute("/_settings/settings/uploads")({ component: UploadSettings });

/**
 * Edits the upload preferences directly in the persistable upload store, so every change
 * is saved and picked up by the running queue without a form-level save step.
 */
function UploadSettings() {
  const { t } = useI18n();
  const settings = useUploadStore((state) => state.settings);
  const setSettings = useUploadStore((state) => state.setSettings);

  return (
    <div className="space-y-6">
      <SettingsPageHeader
        title={t("settings.uploads.title")}
        description={t("settings.uploads.description")}
      />
      <SettingsSection
        title={t("settings.uploads.behavior.section")}
        description={t("settings.uploads.behavior.description")}
      >
        <SettingsRow
          label={t("settings.uploads.encryption.label")}
          description={t("settings.uploads.encryption.description")}
        >
          <Switch
            aria-label={t("settings.uploads.encryption.toggle")}
            isSelected={settings.encryption}
            onChange={(isSelected) => setSettings({ encryption: isSelected })}
          >
            <Switch.Content>
              <Switch.Control>
                <Switch.Thumb />
              </Switch.Control>
              <Label>{t("settings.uploads.encryption.toggle")}</Label>
            </Switch.Content>
          </Switch>
        </SettingsRow>
        <SettingsRow
          label={t("settings.uploads.conflicts.label")}
          description={t("settings.uploads.conflicts.description")}
        >
          <Select
            aria-label={t("settings.uploads.conflicts.label")}
            selectedKey={settings.conflictPolicy}
            onSelectionChange={(key) =>
              setSettings({ conflictPolicy: String(key) as typeof settings.conflictPolicy })
            }
          >
            <Select.Trigger>
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                <ListBox.Item id="rename" textValue={t("settings.uploads.conflicts.rename")}>
                  {t("settings.uploads.conflicts.rename")}
                </ListBox.Item>
                <ListBox.Item id="replace" textValue={t("settings.uploads.conflicts.replace")}>
                  {t("settings.uploads.conflicts.replace")}
                </ListBox.Item>
                <ListBox.Item id="error" textValue={t("settings.uploads.conflicts.error")}>
                  {t("settings.uploads.conflicts.error")}
                </ListBox.Item>
              </ListBox>
            </Select.Popover>
          </Select>
        </SettingsRow>
        <SettingsRow
          label={t("settings.uploads.concurrency.label")}
          description={t("settings.uploads.concurrency.description")}
        >
          <NumberField
            aria-label={t("settings.uploads.concurrency.label")}
            value={settings.concurrency}
            minValue={1}
            maxValue={12}
            onChange={(value) =>
              // Re-clamped here as well as by the field's own bounds: the number input can
              // still emit an out-of-range value when it is typed rather than stepped.
              setSettings({ concurrency: Math.max(1, Math.min(12, value ?? 1)) })
            }
          >
            <Label className="sr-only">{t("settings.uploads.concurrency.label")}</Label>
            <NumberField.Group>
              <NumberField.DecrementButton />
              <NumberField.Input />
              <NumberField.IncrementButton />
            </NumberField.Group>
          </NumberField>
        </SettingsRow>
        <SettingsRow
          label={t("settings.uploads.partSize.label")}
          description={t("settings.uploads.partSize.description")}
        >
          <PartSizeField />
        </SettingsRow>
      </SettingsSection>
    </div>
  );
}

/**
 * Part-size input, edited in MiB and committed on blur rather than per keystroke: each
 * commit re-normalises the number (16 MiB steps, 16 MiB..`MAX_PART_SIZE_MIB`), and doing
 * that mid-typing would rewrite the digits under the cursor.
 */
function PartSizeField() {
  const { t } = useI18n();
  const preferredPartSize = useUploadStore((state) => state.settings.preferredPartSize);
  const setSettings = useUploadStore((state) => state.setSettings);
  const [value, setValue] = useState(preferredPartSize / 1024 / 1024);
  // Mirrors the displayed value so `commit` normalises the number last typed, not the one
  // from the render that produced the blur handler.
  const valueRef = useRef(value);

  const commit = () => {
    const normalized = normalizePartSizeMiB(valueRef.current);
    valueRef.current = normalized;
    setValue(normalized);
    // The store keeps bytes; the field and the normaliser work in MiB.
    setSettings({ preferredPartSize: normalized * 1024 * 1024 });
  };

  return (
    <NumberField
      aria-label={t("settings.uploads.partSize.input")}
      value={value}
      maxValue={MAX_PART_SIZE_MIB}
      onChange={(next) => {
        // An emptied field counts as the 512 MiB default; nothing reaches the store until
        // the field is blurred.
        valueRef.current = next ?? 512;
        setValue(valueRef.current);
      }}
      onBlur={commit}
    >
      <Label className="sr-only">{t("settings.uploads.partSize.input")}</Label>
      <NumberField.Group>
        <NumberField.DecrementButton />
        <NumberField.Input />
        <span className="pr-2 text-xs text-muted">MiB</span>
        <NumberField.IncrementButton />
      </NumberField.Group>
    </NumberField>
  );
}
