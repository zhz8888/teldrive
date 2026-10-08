import { Chip } from "@heroui/react";
import { type MessageKey, t, useI18n } from "@/lib/i18n";

/** Statuses that are still in flight; only these get the pulsing dot. */
const ACTIVE_STATUSES = new Set(["pending", "scheduled", "available", "running", "retryable"]);

/**
 * Statuses the interface names explicitly. Anything else (a job type this build
 * does not know) falls back to the raw value, only capitalized.
 */
const STATUS_KEYS: Partial<Record<string, MessageKey>> = {
  pending: "components.taskStatus.pending",
  scheduled: "components.taskStatus.scheduled",
  available: "components.taskStatus.available",
  running: "components.taskStatus.running",
  retryable: "components.taskStatus.retryable",
  completed: "components.taskStatus.completed",
  discarded: "components.taskStatus.discarded",
  cancelled: "components.taskStatus.cancelled",
};

/** Translates a known status, falling back to the raw value capitalized. */
function statusLabel(translate: (key: MessageKey) => string, status: string) {
  const key = STATUS_KEYS[status];
  if (key) return translate(key);
  return status.charAt(0).toUpperCase() + status.slice(1);
}

/** Localized status label for callers that are not rendering the chip. */
export function taskStatusLabel(status: string) {
  return statusLabel(t, status);
}

/**
 * Coloured chip naming a job status, with a pulsing dot while the job is still
 * active. `animate` turns the pulse off where motion would distract.
 */
export function TaskStatusChip({ status, animate = true }: { status: string; animate?: boolean }) {
  const { t } = useI18n();
  const active = ACTIVE_STATUSES.has(status);

  return (
    <Chip size="sm" variant="soft" color={taskStatusColor(status)}>
      <span className="flex items-center gap-1.5">
        <span className="relative flex size-2 shrink-0" aria-hidden="true">
          {active && animate ? (
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-current opacity-30 motion-reduce:animate-none" />
          ) : null}
          <span className="relative inline-flex size-2 rounded-full bg-current" />
        </span>
        {statusLabel(t, status)}
      </span>
    </Chip>
  );
}

/** Chip colour for a job status; anything unrecognized uses the neutral colour. */
function taskStatusColor(status: string): "accent" | "success" | "warning" | "danger" | "default" {
  switch (status) {
    case "completed":
      return "success";
    case "running":
      return "accent";
    case "pending":
    case "scheduled":
    case "available":
    case "retryable":
      return "warning";
    case "discarded":
      return "danger";
    default:
      return "default";
  }
}
