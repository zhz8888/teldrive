import { Button, Card, Label, ListBox, NumberField, Select, Typography } from "@heroui/react";
import { useStore } from "@tanstack/react-form";
import { toast } from "sonner";
import CloseIcon from "~icons/gravity-ui/xmark";
import type { components } from "@/api/schema";
import { fetchClient } from "@/api/client";
import { type MessageKey, useI18n } from "@/lib/i18n";
import { useAppForm } from "../forms/app-form";

/** Request body of `POST /v1/jobs`, filled from the launcher form. */
type JobCreate = components["schemas"]["JobCreate"];
/** Job kinds the launcher offers; every entry in {@link GROUPS} names one. */
type TaskType = "teldrive_upload_cleanup" | "teldrive_pending_file_purge";

/** Launcher form values, split between the job's `args` and its River options. */
type TaskFormValues = {
  /** Job kind to queue; selecting one also resets the kind's defaults. */
  taskType: TaskType;
  /** `args.batchSize`: batch size sent with the job, at least 1. */
  batchSize: number;
  /** Target queue; blank falls back to "maintenance" when the job is sent. */
  queue: string;
  /** River priority, 1 being the highest and 4 the lowest. */
  priority: number;
  /** River attempt budget before the job is discarded. */
  maxAttempts: number;
};

/**
 * Task kinds grouped for the sidebar: display labels plus the queue, priority
 * and attempt defaults applied when the kind is selected.
 */
const GROUPS: {
  labelKey: MessageKey;
  items: {
    key: TaskType;
    labelKey: MessageKey;
    descriptionKey: MessageKey;
    defaultQueue: string;
  }[];
}[] = [
  {
    labelKey: "components.taskLauncher.groupMaintenance",
    items: [
      {
        key: "teldrive_upload_cleanup",
        labelKey: "components.taskLauncher.uploadCleanup",
        descriptionKey: "components.taskLauncher.uploadCleanupDescription",
        defaultQueue: "maintenance",
      },
      {
        key: "teldrive_pending_file_purge",
        labelKey: "components.taskLauncher.pendingPurge",
        descriptionKey: "components.taskLauncher.pendingPurgeDescription",
        defaultQueue: "maintenance",
      },
    ],
  },
];

/** Values the form opens with; `chooseTask` replaces the per-kind fields. */
const DEFAULT_VALUES: TaskFormValues = {
  taskType: "teldrive_upload_cleanup",
  batchSize: 100,
  queue: "maintenance",
  priority: 2,
  maxAttempts: 10,
};

/**
 * Slide-over form that queues one maintenance job. `onQueued` runs after the
 * API accepted the request, so the caller can refresh its task list, and
 * `onClose` is the only way to dismiss the panel; a rejected request keeps the
 * panel open with the values the user entered.
 */
export function TaskLauncher({ onQueued, onClose }: { onQueued: () => void; onClose: () => void }) {
  const { t } = useI18n();
  const form = useAppForm({
    defaultValues: DEFAULT_VALUES,
    validators: {
      onSubmit: ({ value }) => {
        if (value.batchSize < 1) return "Batch size must be at least 1";
        return undefined;
      },
    },
    onSubmit: async ({ value }) => {
      const body: JobCreate = {
        type: value.taskType,
        args: { batchSize: value.batchSize },
        queue: value.queue.trim() || "maintenance",
        priority: value.priority,
        maxAttempts: value.maxAttempts,
        tags: ["teldrive", "maintenance"],
      };
      try {
        await fetchClient.POST("/v1/jobs", { body });
      } catch {
        // The launcher stays open with the values the user entered.
        toast.error(t("components.taskLauncher.queueFailed"));
        return;
      }
      toast.success(t("components.taskLauncher.queued"));
      onQueued();
    },
  });
  const values = useStore(form.store, (state) => state.values);
  const submitting = useStore(form.store, (state) => state.isSubmitting);
  // Every selectable kind comes from GROUPS, so the lookup always resolves.
  const selected = GROUPS.flatMap((group) => group.items).find(
    (item) => item.key === values.taskType,
  )!;

  const chooseTask = (taskType: TaskType) => {
    const task = GROUPS.flatMap((group) => group.items).find((item) => item.key === taskType)!;
    form.setFieldValue("taskType", taskType);
    form.setFieldValue("queue", task.defaultQueue);
    form.setFieldValue("priority", taskType === "teldrive_upload_cleanup" ? 2 : 1);
    form.setFieldValue("maxAttempts", taskType === "teldrive_upload_cleanup" ? 10 : 25);
  };

  return (
    <form.AppForm>
      <div className="fixed inset-0 z-50 flex justify-end">
        <Button
          type="button"
          variant="ghost"
          aria-label={t("components.taskLauncher.close")}
          className="absolute inset-0 h-full w-full rounded-none bg-black/40 backdrop-blur-[1px]"
          onPress={onClose}
        />
        <form
          className="relative z-10 flex h-full w-full max-w-[52rem] flex-col border-border border-l bg-background shadow-2xl"
          onSubmit={(event) => {
            event.preventDefault();
            void form.handleSubmit();
          }}
        >
          <div className="flex items-start justify-between border-border border-b px-5 py-4 sm:px-6">
            <div>
              <h2 className="text-lg font-semibold">{t("components.taskLauncher.title")}</h2>
              <p className="mt-0.5 text-xs text-muted">{t("components.taskLauncher.subtitle")}</p>
            </div>
            <Button
              isIconOnly
              size="sm"
              variant="tertiary"
              aria-label={t("components.taskLauncher.close")}
              onPress={onClose}
            >
              <CloseIcon className="size-4" />
            </Button>
          </div>

          <div className="min-h-0 flex-1 overflow-hidden">
            <div className="border-border border-b p-4 md:hidden">
              <Select
                aria-label={t("components.taskLauncher.taskType")}
                selectedKey={values.taskType}
                onSelectionChange={(key) => chooseTask(String(key) as TaskType)}
              >
                <Label>{t("components.taskLauncher.taskType")}</Label>
                <Select.Trigger>
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    {GROUPS.flatMap((group) => group.items).map((item) => (
                      <ListBox.Item key={item.key} id={item.key} textValue={t(item.labelKey)}>
                        <div className="min-w-0">
                          <div className="text-sm font-medium">{t(item.labelKey)}</div>
                          <div className="truncate text-xs text-muted">
                            {t(item.descriptionKey)}
                          </div>
                        </div>
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
              </Select>
            </div>

            <div className="grid h-full min-h-0 md:grid-cols-[12rem_minmax(0,1fr)]">
              <nav
                className="hidden overflow-y-auto border-border border-r px-3 py-4 md:block"
                aria-label={t("components.taskLauncher.taskType")}
              >
                {GROUPS.map((group) => (
                  <div key={group.labelKey} className="mb-5 last:mb-0">
                    <div className="mb-1.5 px-2 text-[10px] font-semibold uppercase tracking-[0.14em] text-muted">
                      {t(group.labelKey)}
                    </div>
                    <div className="grid gap-0.5">
                      {group.items.map((item) => {
                        const active = values.taskType === item.key;
                        return (
                          <Button
                            key={item.key}
                            type="button"
                            size="sm"
                            variant="ghost"
                            aria-current={active ? "page" : undefined}
                            className={`relative h-9 w-full justify-start rounded-md px-2.5 text-left ${
                              active
                                ? "bg-accent/12 text-accent"
                                : "text-muted hover:text-foreground"
                            }`}
                            onPress={() => chooseTask(item.key)}
                          >
                            {t(item.labelKey)}
                          </Button>
                        );
                      })}
                    </div>
                  </div>
                ))}
              </nav>

              <div className="min-h-0 overflow-y-auto p-5 sm:p-6">
                <div className="grid gap-5">
                  <div>
                    <Typography type="h3" className="text-base font-semibold">
                      {t(selected.labelKey)}
                    </Typography>
                    <Typography.Paragraph className="mt-1 text-sm text-muted">
                      {t(selected.descriptionKey)}
                    </Typography.Paragraph>
                  </div>

                  <Card className="grid gap-4 bg-surface-secondary p-4">
                    <NumberField
                      value={values.batchSize}
                      minValue={1}
                      maxValue={1000}
                      onChange={(value) => form.setFieldValue("batchSize", value ?? 1)}
                    >
                      <Label>{t("components.taskLauncher.batchSize")}</Label>
                      <NumberField.Group>
                        <NumberField.DecrementButton />
                        <NumberField.Input />
                        <NumberField.IncrementButton />
                      </NumberField.Group>
                    </NumberField>
                    <form.AppField name="queue">
                      {(field) => (
                        <field.TextField
                          label={t("components.taskLauncher.queue")}
                          description={t("components.taskLauncher.queueDescription")}
                        />
                      )}
                    </form.AppField>
                    <div className="grid gap-4 sm:grid-cols-2">
                      <NumberField
                        value={values.priority}
                        minValue={1}
                        maxValue={4}
                        onChange={(value) => form.setFieldValue("priority", value ?? 1)}
                      >
                        <Label>{t("components.taskLauncher.priority")}</Label>
                        <NumberField.Group>
                          <NumberField.DecrementButton />
                          <NumberField.Input />
                          <NumberField.IncrementButton />
                        </NumberField.Group>
                      </NumberField>
                      <NumberField
                        value={values.maxAttempts}
                        minValue={1}
                        onChange={(value) => form.setFieldValue("maxAttempts", value ?? 1)}
                      >
                        <Label>{t("components.taskLauncher.maxAttempts")}</Label>
                        <NumberField.Group>
                          <NumberField.DecrementButton />
                          <NumberField.Input />
                          <NumberField.IncrementButton />
                        </NumberField.Group>
                      </NumberField>
                    </div>
                  </Card>
                </div>
              </div>
            </div>
          </div>

          <div className="flex items-center justify-end gap-2 border-border border-t px-5 py-4 sm:px-6">
            <Button type="button" variant="tertiary" isDisabled={submitting} onPress={onClose}>
              {t("common.action.cancel")}
            </Button>
            <form.SubmitButton variant="primary">
              {t("components.taskLauncher.queueTask", { task: t(selected.labelKey).toLowerCase() })}
            </form.SubmitButton>
          </div>
        </form>
      </div>
    </form.AppForm>
  );
}
