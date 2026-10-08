import { Button, Card, Chip, Label, ListBox, NumberField, Select, Typography } from "@heroui/react";
import { useStore } from "@tanstack/react-form";
import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { $api as api } from "@/api/client";
import { queryClient } from "@/api/query-client";
import type { components } from "@/api/schema";
import { type MessageKey, type MessageParams, t as translate, useI18n } from "@/lib/i18n";
import PauseIcon from "~icons/gravity-ui/pause";
import EditIcon from "~icons/gravity-ui/pencil";
import PlayIcon from "~icons/gravity-ui/play";
import AddIcon from "~icons/gravity-ui/plus";
import TrashBinIcon from "~icons/gravity-ui/trash-bin";
import ResetIcon from "~icons/material-symbols/restart-alt-rounded";
import { AppDialog } from "../components/dialogs/app-dialog";
import { ConfirmDialog } from "../components/dialogs/confirm-dialog";
import { SettingsPageHeader } from "../components/settings-layout";
import { useAppForm } from "../forms/app-form";

/** A registered periodic job as the API returns it, including its next run and pause state. */
type PeriodicJob = components["schemas"]["PeriodicJob"];
/** A catalog entry describing one worker kind and the defaults it suggests. */
type PeriodicJobTemplate = components["schemas"]["PeriodicJobTemplate"];
/** Body of a create call; `id` names the job and `paused` decides whether it starts paused. */
type PeriodicJobCreateRequest = components["schemas"]["PeriodicJobCreate"];
/** Body of an update call: the same fields as a create, minus the id. */
type PeriodicJobUpdateRequest = components["schemas"]["PeriodicJobUpdate"];

/**
 * Editor state for one periodic job. It mirrors the API body but in the shapes a form can
 * hold: arguments are JSON text, tags are comma-separated, and `id` is present even when
 * editing, where the field is shown read-only.
 */
type JobFormValues = {
  /** Job id; the create call's key, and read-only once the job exists. */
  id: string;
  /** Registered worker kind to run. */
  kind: string;
  /** Arguments as JSON text, parsed on submit rather than on each keystroke. */
  argsText: string;
  /** Queue the job is inserted into. */
  queue: string;
  /** River priority, 1 (highest) to 4. */
  priority: number;
  /** Attempts before the job is discarded. */
  maxAttempts: number;
  /** Tags as a comma-separated list, split and trimmed on submit. */
  tagsText: string;
  /** Cron expression in `cronTimezone`, not in the browser's zone. */
  cronExpression: string;
  /** IANA zone the expression is evaluated in; "UTC" when left blank. */
  cronTimezone: string;
  /** Whether the job starts paused; only sent when creating. */
  paused: boolean;
};

/**
 * The blank form, used when the editor opens with neither a job to edit nor a template to
 * seed from, and the base that every seeded form spreads over. Its values are the client's
 * own starting point — queue "cron", highest priority, 25 attempts, daily at midnight UTC —
 * rather than anything read back from the server.
 */
const EMPTY_JOB: JobFormValues = {
  id: "",
  kind: "",
  argsText: "{}",
  queue: "cron",
  priority: 1,
  maxAttempts: 25,
  tagsText: "",
  cronExpression: "0 0 * * *",
  cronTimezone: "UTC",
  paused: false,
};

/**
 * Ready-made schedules offered as one-click buttons, with the expression each one stands
 * for. They are ordinary five-field cron in the job's own timezone, and the buttons also
 * highlight when the current expression matches one of them.
 */
const CRON_PRESETS = [
  { labelKey: "settings.periodicJobs.preset.hourly", value: "0 * * * *" },
  { labelKey: "settings.periodicJobs.preset.every2Hours", value: "0 */2 * * *" },
  { labelKey: "settings.periodicJobs.preset.every6Hours", value: "0 */6 * * *" },
  { labelKey: "settings.periodicJobs.preset.daily", value: "0 0 * * *" },
  { labelKey: "settings.periodicJobs.preset.weekly", value: "0 0 * * 0" },
] as const;

/**
 * `/settings/periodic-jobs` — the cron-driven job schedules. The loader warms both the
 * schedule list and the worker catalog, because the editor's template picker is populated
 * from the catalog as soon as it opens.
 */
export const Route = createFileRoute("/_settings/settings/periodic-jobs")({
  loader: async () => {
    await Promise.all([
      queryClient.ensureQueryData(api.queryOptions("get", "/v1/periodic-jobs")),
      queryClient.ensureQueryData(api.queryOptions("get", "/v1/periodic-jobs/catalog")),
    ]);
  },
  component: PeriodicJobsPage,
});

/**
 * Lists the periodic jobs and owns every mutation on them: create, update, delete, pause,
 * resume and the bulk reset. Each mutation refreshes the list on success, because the
 * server recomputes the next run time and the computed state is what the cards show.
 */
function PeriodicJobsPage() {
  const { t } = useI18n();
  const { data: jobsResponse } = api.useSuspenseQuery("get", "/v1/periodic-jobs");
  const { data: catalogResponse } = api.useSuspenseQuery("get", "/v1/periodic-jobs/catalog");
  const jobs = jobsResponse.jobs ?? [];
  const templates = catalogResponse.templates ?? [];

  // Dialog state: which job the editor holds (null means "create"), which one the delete
  // dialog asks about, and whether the bulk reset is confirming.
  const [editorOpen, setEditorOpen] = useState(false);
  const [editingJob, setEditingJob] = useState<PeriodicJob | null>(null);
  const [deleteJob, setDeleteJob] = useState<PeriodicJob | null>(null);
  const [resetOpen, setResetOpen] = useState(false);

  /** Refetches the schedule list; every mutation below calls it on success. */
  const invalidate = () =>
    queryClient.invalidateQueries({
      queryKey: api.queryOptions("get", "/v1/periodic-jobs").queryKey,
    });

  const createJob = api.useMutation("post", "/v1/periodic-jobs", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.created"));
      setEditorOpen(false);
      void invalidate();
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.createFailed")),
  });
  const updateJob = api.useMutation("put", "/v1/periodic-jobs/{periodicJobId}", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.updated"));
      setEditorOpen(false);
      void invalidate();
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.updateFailed")),
  });
  const deleteMutation = api.useMutation("delete", "/v1/periodic-jobs/{periodicJobId}", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.deleted"));
      setDeleteJob(null);
      void invalidate();
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.deleteFailed")),
  });
  const pauseMutation = api.useMutation("post", "/v1/periodic-jobs/{periodicJobId}/pause", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.paused"));
      void invalidate();
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.pauseFailed")),
  });
  const resumeMutation = api.useMutation("post", "/v1/periodic-jobs/{periodicJobId}/resume", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.resumed"));
      void invalidate();
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.resumeFailed")),
  });
  const resetMutation = api.useMutation("post", "/v1/periodic-jobs/reset", {
    onSuccess: () => {
      toast.success(t("settings.periodicJobs.toast.reset"));
      setResetOpen(false);
      // `resetQueries` drops the cached list before refetching it, so nothing from the
      // previous registration can survive a reset that removed schedules.
      void queryClient.resetQueries({
        queryKey: api.queryOptions("get", "/v1/periodic-jobs").queryKey,
      });
    },
    onError: () => toast.error(t("settings.periodicJobs.toast.resetFailed")),
  });

  /** Opens the editor on a blank form; `editingJob` being null is what selects create mode. */
  const openCreate = () => {
    setEditingJob(null);
    setEditorOpen(true);
  };
  /** Opens the editor on an existing schedule, whose id becomes read-only. */
  const openEdit = (job: PeriodicJob) => {
    setEditingJob(job);
    setEditorOpen(true);
  };

  /**
   * Turns the form values into an API body and submits them. Creating sends the id and the
   * initial pause flag; editing updates the job in place with a body that carries no
   * `paused`, which the endpoint reads as "not paused" — so saving an edit to a paused
   * schedule resumes it.
   */
  const submitJob = async (values: JobFormValues) => {
    const args = parseArguments(values.argsText);
    const common = {
      kind: values.kind.trim(),
      args,
      queue: values.queue.trim(),
      priority: values.priority,
      maxAttempts: values.maxAttempts,
      tags: values.tagsText
        .split(",")
        .map((tag) => tag.trim())
        .filter(Boolean),
      cronExpression: values.cronExpression.trim(),
      cronTimezone: values.cronTimezone.trim() || "UTC",
    } satisfies PeriodicJobUpdateRequest;

    if (editingJob) {
      await updateJob.mutateAsync({
        params: { path: { periodicJobId: editingJob.id ?? values.id } },
        body: common,
      });
      return;
    }
    const body: PeriodicJobCreateRequest = {
      ...common,
      id: values.id.trim(),
      paused: values.paused,
    };
    await createJob.mutateAsync({ body });
  };

  return (
    <div className="flex flex-col gap-5">
      <SettingsPageHeader
        title={t("settings.periodicJobs.title")}
        description={t("settings.periodicJobs.description")}
        actions={
          <div className="flex items-center gap-2">
            <Button variant="tertiary" onPress={() => setResetOpen(true)}>
              <ResetIcon className="size-4" /> {t("settings.periodicJobs.reset")}
            </Button>
            <Button variant="primary" onPress={openCreate}>
              <AddIcon className="size-4" /> {t("settings.periodicJobs.add")}
            </Button>
          </div>
        }
      />

      {jobs.length === 0 ? (
        <Card className="flex min-h-44 items-center justify-center border border-border bg-surface p-6 text-center shadow-none">
          <div className="max-w-md">
            <Typography type="h3" className="text-sm font-semibold">
              {t("settings.periodicJobs.empty.title")}
            </Typography>
            <Typography.Paragraph className="mt-2 text-sm text-muted">
              {t("settings.periodicJobs.empty.description")}
            </Typography.Paragraph>
            <Button className="mt-4" size="sm" variant="primary" onPress={openCreate}>
              <AddIcon className="size-4" /> {t("settings.periodicJobs.empty.action")}
            </Button>
          </div>
        </Card>
      ) : (
        <div className="grid gap-3">
          {jobs.map((job) => (
            <PeriodicJobCard
              key={job.id}
              job={job}
              // A toggle is pending for this card when either mutation is in flight for
              // this job's id; the two are separate operations on the same row.
              isToggling={
                (pauseMutation.isPending &&
                  pauseMutation.variables?.params.path.periodicJobId === job.id) ||
                (resumeMutation.isPending &&
                  resumeMutation.variables?.params.path.periodicJobId === job.id)
              }
              onEdit={() => openEdit(job)}
              onDelete={() => setDeleteJob(job)}
              onToggle={() => {
                const id = job.id ?? "";
                if (job.paused) resumeMutation.mutate({ params: { path: { periodicJobId: id } } });
                else pauseMutation.mutate({ params: { path: { periodicJobId: id } } });
              }}
            />
          ))}
        </div>
      )}

      <PeriodicJobEditor
        open={editorOpen}
        editingJob={editingJob}
        templates={templates}
        onOpenChange={setEditorOpen}
        onSubmit={submitJob}
      />

      <ConfirmDialog
        open={Boolean(deleteJob)}
        onOpenChange={(open) => {
          if (!open) setDeleteJob(null);
        }}
        title={t("settings.periodicJobs.delete.title")}
        message={t("settings.periodicJobs.delete.message", { id: deleteJob?.id ?? "" })}
        confirmLabel={t("settings.periodicJobs.delete.confirm")}
        isPending={deleteMutation.isPending}
        onConfirm={() => {
          if (deleteJob?.id)
            deleteMutation.mutate({ params: { path: { periodicJobId: deleteJob.id } } });
        }}
      />

      <ConfirmDialog
        open={resetOpen}
        onOpenChange={setResetOpen}
        title={t("settings.periodicJobs.resetConfirm.title")}
        message={t("settings.periodicJobs.resetConfirm.message")}
        confirmLabel={t("settings.periodicJobs.reset")}
        isPending={resetMutation.isPending}
        onConfirm={() => resetMutation.mutate({})}
      />
    </div>
  );
}

/**
 * Summary card for one schedule: its id, kind, pause state and the four facts an operator
 * checks first, with the pause/resume, edit and delete actions beside them.
 */
function PeriodicJobCard({
  job,
  isToggling,
  onEdit,
  onDelete,
  onToggle,
}: {
  job: PeriodicJob;
  isToggling: boolean;
  onEdit: () => void;
  onDelete: () => void;
  onToggle: () => void;
}) {
  const { t } = useI18n();
  return (
    <Card className="border border-border bg-surface p-4 shadow-none">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <Typography type="h3" className="truncate text-sm font-semibold">
              {job.id}
            </Typography>
            <Chip size="sm" variant="tertiary" color={job.paused ? "warning" : "success"}>
              {job.paused
                ? t("settings.periodicJobs.status.paused")
                : t("settings.periodicJobs.status.active")}
            </Chip>
            <Chip size="sm" variant="tertiary">
              {job.kind}
            </Chip>
          </div>
          <div className="mt-3 grid gap-2 text-xs sm:grid-cols-2 lg:grid-cols-4">
            <JobDetail
              label={t("settings.periodicJobs.detail.schedule")}
              value={`${job.cronExpression ?? "—"} (${job.cronTimezone ?? "UTC"})`}
              mono
            />
            <JobDetail
              label={t("settings.periodicJobs.detail.nextRun")}
              value={
                job.paused
                  ? t("settings.periodicJobs.status.paused")
                  : formatDateTime(job.nextRunAt)
              }
            />
            <JobDetail
              label={t("settings.periodicJobs.detail.queue")}
              value={job.queue ?? "default"}
            />
            <JobDetail
              label={t("settings.periodicJobs.detail.attempts")}
              value={String(job.maxAttempts ?? 25)}
            />
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap gap-2 lg:justify-end">
          <Button
            isIconOnly
            size="sm"
            variant="tertiary"
            aria-label={
              job.paused
                ? t("settings.periodicJobs.aria.resume", { id: job.id })
                : t("settings.periodicJobs.aria.pause", { id: job.id })
            }
            isPending={isToggling}
            onPress={onToggle}
          >
            {job.paused ? <PlayIcon className="size-4" /> : <PauseIcon className="size-4" />}
          </Button>
          <Button
            isIconOnly
            size="sm"
            variant="tertiary"
            aria-label={t("settings.periodicJobs.aria.edit", { id: job.id })}
            onPress={onEdit}
          >
            <EditIcon className="size-4" />
          </Button>
          <Button
            isIconOnly
            size="sm"
            variant="danger-soft"
            aria-label={t("settings.periodicJobs.aria.delete", { id: job.id })}
            onPress={onDelete}
          >
            <TrashBinIcon className="size-4" />
          </Button>
        </div>
      </div>
    </Card>
  );
}

/** Label/value pair inside a schedule card; `mono` is for the cron expression. */
function JobDetail({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <div className="text-muted">{label}</div>
      <div
        className={
          mono ? "mt-0.5 truncate font-mono text-foreground" : "mt-0.5 truncate text-foreground"
        }
      >
        {value}
      </div>
    </div>
  );
}

/**
 * Create/edit dialog for a schedule. It is a TanStack Form whose fields are declared here
 * and whose initial values come from the job being edited, from the first catalog template
 * or from `EMPTY_JOB`; validation runs on submit only, because arguments are JSON text that
 * is invalid for most of the time it is being typed.
 */
function PeriodicJobEditor({
  open,
  editingJob,
  templates,
  onOpenChange,
  onSubmit,
}: {
  open: boolean;
  editingJob: PeriodicJob | null;
  templates: PeriodicJobTemplate[];
  onOpenChange: (open: boolean) => void;
  onSubmit: (values: JobFormValues) => Promise<void>;
}) {
  const { t } = useI18n();
  const editing = Boolean(editingJob);
  // Recomputed only when the dialog's subject changes; the form is reset to it on open.
  const initialValues = useMemo(
    () =>
      editingJob
        ? formFromJob(editingJob)
        : templates[0]
          ? formFromTemplate(templates[0])
          : EMPTY_JOB,
    [editingJob, templates],
  );
  const form = useAppForm({
    defaultValues: initialValues,
    validators: {
      onSubmit: ({ value }) => {
        const fields: Partial<Record<keyof JobFormValues, string>> = {};
        if (!editing && !value.id.trim())
          fields.id = t("settings.periodicJobs.editor.jobId.required");
        if (!value.kind.trim()) fields.kind = t("settings.periodicJobs.editor.workerKind.required");
        if (!value.cronExpression.trim())
          fields.cronExpression = t("settings.periodicJobs.editor.cronExpression.required");
        try {
          parseArguments(value.argsText);
        } catch (error) {
          fields.argsText =
            error instanceof Error
              ? error.message
              : translate("settings.periodicJobs.editor.arguments.invalidJson");
        }
        return Object.keys(fields).length ? { fields } : undefined;
      },
    },
    onSubmit: async ({ value }) => onSubmit(value),
  });
  // The dialog stays mounted between openings, so the form is re-seeded every time it is
  // opened; otherwise the previous job's values would still be in the fields.
  useEffect(() => {
    if (open) form.reset(initialValues);
  }, [form, initialValues, open]);

  const values = useStore(form.store, (state) => state.values);
  const isSubmitting = useStore(form.store, (state) => state.isSubmitting);
  const cronExpression = values.cronExpression;
  // Plain-language rendering of the current expression, shown under the section heading.
  const scheduleDescription = useMemo(() => describeCron(cronExpression, t), [cronExpression, t]);

  /**
   * Applies a template's defaults to the form. The pause flag is always kept, and so is the
   * id when editing; a new job instead takes the template's suggested id, falling back to
   * whatever was typed when the template suggests none.
   */
  const chooseTemplate = (kind: string) => {
    const template = templates.find((item) => item.kind === kind);
    if (!template) {
      form.setFieldValue("kind", kind);
      return;
    }
    const current = values;
    form.reset({
      ...formFromTemplate(template),
      id: editing ? current.id : (template.defaultId ?? current.id),
      paused: current.paused,
    });
  };

  return (
    <form.AppForm>
      <AppDialog
        className="sm:w-[min(92vw,50rem)]"
        open={open}
        onOpenChange={onOpenChange}
        title={
          editing ? t("settings.periodicJobs.editor.editTitle") : t("settings.periodicJobs.add")
        }
        description={t("settings.periodicJobs.editor.description")}
        bodyClassName="overflow-x-hidden px-2"
        isDismissable={!isSubmitting}
        footer={
          <>
            <Button
              type="button"
              variant="tertiary"
              isDisabled={form.state.isSubmitting}
              onPress={() => onOpenChange(false)}
            >
              {t("common.action.cancel")}
            </Button>
            <form.SubmitButton form="periodic-job-form" variant="primary">
              {editing
                ? t("settings.periodicJobs.editor.save")
                : t("settings.periodicJobs.editor.create")}
            </form.SubmitButton>
          </>
        }
      >
        <form
          id="periodic-job-form"
          className="grid min-w-0 gap-5 py-1"
          onSubmit={(event) => {
            event.preventDefault();
            void form.handleSubmit();
          }}
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <Select
              selectedKey={values.kind}
              onSelectionChange={(key) => chooseTemplate(String(key))}
            >
              <Label>{t("settings.periodicJobs.editor.template")}</Label>
              <Select.Trigger>
                <Select.Value />
                <Select.Indicator />
              </Select.Trigger>
              <Select.Popover>
                <ListBox>
                  {templates.map((template) => (
                    <ListBox.Item
                      key={template.kind}
                      id={template.kind ?? ""}
                      textValue={template.label ?? template.kind}
                    >
                      <div>
                        <div className="text-sm font-medium">{template.label ?? template.kind}</div>
                        <div className="text-xs text-muted">{template.description}</div>
                      </div>
                    </ListBox.Item>
                  ))}
                </ListBox>
              </Select.Popover>
            </Select>
            <form.AppField name="id">
              {(field) => (
                <field.TextField
                  label={t("settings.periodicJobs.editor.jobId")}
                  isRequired
                  isDisabled={editing}
                  description={t("settings.periodicJobs.editor.jobId.description")}
                />
              )}
            </form.AppField>
          </div>

          <form.AppField name="kind">
            {(field) => (
              <field.TextField
                label={t("settings.periodicJobs.editor.workerKind")}
                isRequired
                description={t("settings.periodicJobs.editor.workerKind.description")}
              />
            )}
          </form.AppField>

          <Card className="gap-4 bg-surface-secondary p-4">
            <div>
              <Typography type="h3" className="text-sm font-semibold">
                {t("settings.periodicJobs.editor.schedule.section")}
              </Typography>
              <Typography.Paragraph className="text-xs text-muted">
                {scheduleDescription}
              </Typography.Paragraph>
            </div>
            <div className="flex flex-wrap gap-2">
              {CRON_PRESETS.map((preset) => (
                <Button
                  key={preset.value}
                  type="button"
                  size="sm"
                  variant={cronExpression === preset.value ? "primary" : "tertiary"}
                  onPress={() => form.setFieldValue("cronExpression", preset.value)}
                >
                  {t(preset.labelKey)}
                </Button>
              ))}
            </div>
            <div className="grid gap-4">
              <form.AppField name="cronExpression">
                {(field) => (
                  <field.TextField
                    label={t("settings.periodicJobs.editor.cronExpression")}
                    isRequired
                    className="font-mono"
                    description={t("settings.periodicJobs.editor.cronExpression.description")}
                  />
                )}
              </form.AppField>
              <form.AppField name="cronTimezone">
                {(field) => (
                  <field.TextField
                    label={t("settings.periodicJobs.editor.timezone")}
                    description={t("settings.periodicJobs.editor.timezone.description")}
                  />
                )}
              </form.AppField>
            </div>
          </Card>

          <div className="grid gap-4 sm:grid-cols-3">
            <form.AppField name="queue">
              {(field) => <field.TextField label={t("settings.periodicJobs.editor.queue")} />}
            </form.AppField>
            <form.AppField name="priority">
              {(field) => (
                <NumberField
                  value={field.state.value}
                  minValue={1}
                  maxValue={4}
                  onChange={(value) => field.handleChange(value ?? 1)}
                >
                  <Label>{t("settings.periodicJobs.editor.priority")}</Label>
                  <NumberField.Group>
                    <NumberField.DecrementButton />
                    <NumberField.Input />
                    <NumberField.IncrementButton />
                  </NumberField.Group>
                </NumberField>
              )}
            </form.AppField>
            <form.AppField name="maxAttempts">
              {(field) => (
                <NumberField
                  value={field.state.value}
                  minValue={1}
                  onChange={(value) => field.handleChange(value ?? 1)}
                >
                  <Label>{t("settings.periodicJobs.editor.maxAttempts")}</Label>
                  <NumberField.Group>
                    <NumberField.DecrementButton />
                    <NumberField.Input />
                    <NumberField.IncrementButton />
                  </NumberField.Group>
                </NumberField>
              )}
            </form.AppField>
          </div>

          <form.AppField name="tagsText">
            {(field) => (
              <field.TextField
                label={t("settings.periodicJobs.editor.tags")}
                placeholder={t("settings.periodicJobs.editor.tags.placeholder")}
                description={t("settings.periodicJobs.editor.tags.description")}
              />
            )}
          </form.AppField>
          <form.AppField name="argsText">
            {(field) => (
              <field.TextAreaField
                label={t("settings.periodicJobs.editor.arguments")}
                className="min-h-52 resize-y font-mono text-sm"
                spellCheck={false}
                description={t("settings.periodicJobs.editor.arguments.description")}
              />
            )}
          </form.AppField>
          {!editing ? (
            <form.AppField name="paused">
              {(field) => (
                <field.SwitchField
                  label={t("settings.periodicJobs.editor.createPaused")}
                  description={t("settings.periodicJobs.editor.createPaused.description")}
                />
              )}
            </form.AppField>
          ) : null}
        </form>
      </AppDialog>
    </form.AppForm>
  );
}

/**
 * Seeds the form from a catalog template, keeping `EMPTY_JOB`'s defaults for everything
 * the template does not suggest (priority, attempts, timezone, pause flag).
 */
function formFromTemplate(template: PeriodicJobTemplate): JobFormValues {
  return {
    ...EMPTY_JOB,
    id: template.defaultId ?? "",
    kind: template.kind ?? "",
    argsText: JSON.stringify(template.defaultArgs ?? {}, null, 2),
    queue: template.defaultQueue ?? "cron",
    cronExpression: template.recommendedCron ?? "0 0 * * *",
  };
}

/**
 * The inverse of `submitJob`: renders a stored schedule into the form, with its arguments
 * pretty-printed as JSON and its tags joined into the comma-separated text the field holds.
 */
function formFromJob(job: PeriodicJob): JobFormValues {
  return {
    id: job.id ?? "",
    kind: job.kind ?? "",
    argsText: JSON.stringify(job.args ?? {}, null, 2),
    queue: job.queue ?? "default",
    priority: job.priority ?? 1,
    maxAttempts: job.maxAttempts ?? 25,
    tagsText: (job.tags ?? []).join(", "),
    cronExpression: job.cronExpression ?? "",
    cronTimezone: job.cronTimezone ?? "UTC",
    paused: job.paused ?? false,
  };
}

/**
 * Parses the arguments field, treating blank text as an empty object. Anything that is not
 * a JSON object — a syntax error, an array, a scalar — throws, and the submit validator
 * reports the thrown message on the field.
 */
function parseArguments(value: string): Record<string, unknown> {
  const parsed: unknown = JSON.parse(value || "{}");
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object")
    throw new Error(translate("settings.periodicJobs.editor.arguments.invalid"));
  return parsed as Record<string, unknown>;
}

/**
 * Describes the current expression: by a preset's own name when one matches, as "empty"
 * when nothing is typed, and by quoting the expression itself otherwise. Nothing is
 * interpreted beyond that — there is no cron parser here.
 */
function describeCron(
  expression: string,
  t: (key: MessageKey, params?: MessageParams) => string,
): string {
  const preset = CRON_PRESETS.find((item) => item.value === expression.trim());
  if (preset) return t("settings.periodicJobs.schedule.preset", { preset: t(preset.labelKey) });
  if (!expression.trim()) return t("settings.periodicJobs.schedule.empty");
  return t("settings.periodicJobs.schedule.custom", { expression: expression.trim() });
}

/**
 * Formats an optional timestamp in the browser's locale. An absent value reads as an em
 * dash, and a value the date parser rejects is shown verbatim rather than as "Invalid Date".
 */
function formatDateTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(
    date,
  );
}
