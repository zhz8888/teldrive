import {
  Accordion,
  Button,
  Input,
  Label,
  NumberField,
  Switch,
  TextArea,
  TextField,
} from "@heroui/react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import PlusIcon from "~icons/gravity-ui/plus";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import type { components } from "@/api/schema";
import { fetchClient } from "@/api/client";
import { userMessage } from "@/api/errors";
import { type MessageKey, type MessageParams, useI18n } from "@/lib/i18n";
import { newClientId } from "@/features/shared/client-id";
import { AppDialog } from "./dialogs/app-dialog";

/** One entry of the import request's `sources` array. */
type ImportSource = components["schemas"]["UploadImportSource"];
/** Request body of `POST /v1/uploads/imports`. */
type ImportRequest = components["schemas"]["UploadImportRequest"];

/** Translator shape, so the module-level header parser can receive one. */
type Translate = (key: MessageKey, params?: MessageParams) => string;

/**
 * Editable import source. `type` decides which field is sent (`path` for local,
 * `url` for http); `exclude` and `headers` stay text until the request is built.
 */
type SourceDraft = {
  /** Row identity for patching and removing it; never sent to the API. */
  id: string;
  /** Source kind; switching it clears `location`, which is validated per kind. */
  type: "local" | "http";
  /** Absolute server path or remote URL, depending on `type`. */
  location: string;
  /** Optional sub-path created inside the destination folder. */
  destinationPath: string;
  /** Exclude globs, one per line. */
  exclude: string;
  /** HTTP headers, one `Name: value` per line; ignored for local sources. */
  headers: string;
};

/** A blank local source row with a fresh id. */
const newSource = (): SourceDraft => ({
  id: newClientId(),
  type: "local",
  location: "",
  destinationPath: "",
  exclude: "",
  headers: "",
});

/**
 * Dialog that queues a batch import of local paths or remote URLs. `currentPath`
 * seeds the destination each time the dialog opens, and the form keeps whatever
 * was entered while the request is in flight: closing is blocked until it
 * settles, so a queued batch cannot be lost by a stray backdrop press.
 */
export function BackgroundUploadDialog({
  open,
  onOpenChange,
  currentPath,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentPath: string;
}) {
  const { t } = useI18n();
  const [destination, setDestination] = useState(currentPath);
  const [sources, setSources] = useState<SourceDraft[]>([newSource()]);
  const [exclude, setExclude] = useState("");
  const [headers, setHeaders] = useState("");
  const [minSize, setMinSize] = useState("");
  const [maxSize, setMaxSize] = useState("");
  // Part size in MiB, sent as bytes: the 64-2000 range is what the backend
  // accepts, and 512 MiB is its default.
  const [chunkSizeMiB, setChunkSizeMiB] = useState(512);
  // Parallel parts per file; the backend rejects a request above 16 and
  // defaults to 4.
  const [partConcurrency, setPartConcurrency] = useState(4);
  const [encryption, setEncryption] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  // Re-seed on every open: the dialog stays mounted while the browsed folder
  // keeps changing underneath it.
  useEffect(() => {
    if (open) setDestination(currentPath);
  }, [currentPath, open]);

  const reset = () => {
    setSources([newSource()]);
    setDestination(currentPath);
    setExclude("");
    setHeaders("");
    setMinSize("");
    setMaxSize("");
    setChunkSizeMiB(512);
    setPartConcurrency(4);
    setEncryption(false);
  };

  const close = () => {
    if (submitting) return;
    onOpenChange(false);
  };

  const patchSource = (id: string, patch: Partial<SourceDraft>) => {
    setSources((items) => items.map((item) => (item.id === id ? { ...item, ...patch } : item)));
  };

  const queueUpload = async () => {
    try {
      const target = destination.trim();
      if (!target) throw new Error(t("components.backgroundUpload.destinationRequired"));
      if (!target.startsWith("/") && !isUUID(target)) {
        throw new Error(t("components.backgroundUpload.destinationInvalid"));
      }
      const bodySources = sources.map<ImportSource>((source, index) => {
        const location = source.location.trim();
        if (!location)
          throw new Error(t("components.backgroundUpload.sourceEmpty", { index: index + 1 }));
        if (source.type === "local" && !location.startsWith("/")) {
          throw new Error(t("components.backgroundUpload.sourceAbsolute", { index: index + 1 }));
        }
        if (source.type === "http") {
          const url = new URL(location);
          if (url.protocol !== "http:" && url.protocol !== "https:") {
            throw new Error(t("components.backgroundUpload.sourceProtocol", { index: index + 1 }));
          }
        }
        const destinationPath = source.destinationPath.trim();
        if (destinationPath.startsWith("/") || destinationPath.split("/").includes("..")) {
          throw new Error(
            t("components.backgroundUpload.destinationRelative", { index: index + 1 }),
          );
        }
        const item: ImportSource = {
          type: source.type,
          destinationPath: destinationPath || undefined,
          exclude: lines(source.exclude),
          headers: parseHeaders(source.headers, t),
        };
        if (source.type === "local") item.path = location;
        else item.url = location;
        return item;
      });
      const body: ImportRequest = {
        destination: target,
        sources: bodySources,
        headers: parseHeaders(headers, t),
        exclude: lines(exclude),
        minSize: minSize.trim() || undefined,
        maxSize: maxSize.trim() || undefined,
        partConcurrency,
        chunkSize: chunkSizeMiB * 1024 * 1024,
        encryption,
      };
      setSubmitting(true);
      try {
        // The client throws for a rejected request, so the failure carries the
        // reason to report instead of the dead `error` field the response never
        // fills.
        await fetchClient.POST("/v1/uploads/imports", { body });
      } catch (error) {
        throw new Error(userMessage(error) || t("components.backgroundUpload.serverRejected"));
      }
      toast.success(t("components.backgroundUpload.queued"));
      reset();
      onOpenChange(false);
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t("components.backgroundUpload.queueFailed"),
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <AppDialog
      open={open}
      onOpenChange={(next) => (next ? onOpenChange(true) : close())}
      isDismissable={!submitting}
      title={t("components.backgroundUpload.title")}
      description={t("components.backgroundUpload.description", { path: currentPath })}
      className="min-w-0 sm:w-[min(94vw,46rem)] sm:max-w-none bg-surface"
      bodyClassName="p-0"
      footer={
        <>
          <Button variant="secondary" isDisabled={submitting} onPress={close}>
            {t("common.action.cancel")}
          </Button>
          <Button variant="primary" isPending={submitting} onPress={() => void queueUpload()}>
            {t("components.backgroundUpload.queue")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-4 sm:p-5">
        <TextField value={destination} onChange={setDestination} isRequired>
          <Label>{t("components.backgroundUpload.destination")}</Label>
          <Input placeholder={t("components.backgroundUpload.destinationPlaceholder")} />
          <div className="mt-1 text-xs text-muted">
            {t("components.backgroundUpload.destinationHint")}
          </div>
        </TextField>

        <div className="grid gap-3">
          {sources.map((source, index) => (
            <section
              key={source.id}
              className="rounded-xl border border-border bg-default/15 p-3 sm:p-4"
            >
              <div className="mb-3 flex items-center justify-between gap-3">
                <div>
                  <div className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">
                    {t("components.backgroundUpload.source", { index: index + 1 })}
                  </div>
                  <div className="mt-0.5 text-xs text-muted">
                    {source.type === "local"
                      ? t("components.backgroundUpload.sourceLocalHint")
                      : t("components.backgroundUpload.sourceHttpHint")}
                  </div>
                </div>
                <Button
                  isIconOnly
                  size="sm"
                  variant="ghost"
                  aria-label={t("components.backgroundUpload.removeSource", { index: index + 1 })}
                  isDisabled={sources.length === 1}
                  onPress={() =>
                    setSources((items) => items.filter((item) => item.id !== source.id))
                  }
                >
                  <TrashIcon className="size-3.5" />
                </Button>
              </div>

              <div className="mb-3 grid grid-cols-2 rounded-lg bg-default/35 p-1">
                {(["local", "http"] as const).map((type) => (
                  <Button
                    key={type}
                    size="sm"
                    variant={source.type === type ? "secondary" : "ghost"}
                    onPress={() => patchSource(source.id, { type, location: "" })}
                  >
                    {type === "local"
                      ? t("components.backgroundUpload.typeLocal")
                      : t("components.backgroundUpload.typeHttp")}
                  </Button>
                ))}
              </div>

              <div className="grid gap-3 sm:grid-cols-[minmax(0,1.45fr)_minmax(0,1fr)]">
                <TextField
                  value={source.location}
                  onChange={(value) => patchSource(source.id, { location: value })}
                >
                  <Label>
                    {source.type === "local"
                      ? t("components.backgroundUpload.absolutePath")
                      : t("components.backgroundUpload.url")}
                  </Label>
                  <Input
                    placeholder={
                      source.type === "local"
                        ? "/srv/media/photos"
                        : "https://example.com/archive.zip"
                    }
                  />
                </TextField>
                <TextField
                  value={source.destinationPath}
                  onChange={(value) => patchSource(source.id, { destinationPath: value })}
                >
                  <Label>{t("components.backgroundUpload.destinationPath")}</Label>
                  <Input
                    placeholder={t("components.backgroundUpload.destinationPathPlaceholder")}
                  />
                </TextField>
              </div>

              <Accordion hideSeparator className="mt-2 w-full">
                <Accordion.Item id={`source-options-${source.id}`}>
                  <Accordion.Heading>
                    <Accordion.Trigger className="rounded-lg text-xs font-medium text-muted hover:text-foreground">
                      {t("components.backgroundUpload.sourceOptions")}
                      <Accordion.Indicator />
                    </Accordion.Trigger>
                  </Accordion.Heading>
                  <Accordion.Panel>
                    <Accordion.Body>
                      <div className="grid gap-3 border-border border-t pt-3 sm:grid-cols-2">
                        <TextField>
                          <Label>{t("components.backgroundUpload.excludePatterns")}</Label>
                          <TextArea
                            value={source.exclude}
                            onChange={(event) =>
                              patchSource(source.id, { exclude: event.currentTarget.value })
                            }
                            placeholder={"*.tmp\n**/.git/**"}
                            rows={3}
                          />
                        </TextField>
                        <TextField isDisabled={source.type !== "http"}>
                          <Label>{t("components.backgroundUpload.httpHeaders")}</Label>
                          <TextArea
                            value={source.headers}
                            onChange={(event) =>
                              patchSource(source.id, { headers: event.currentTarget.value })
                            }
                            placeholder={"Authorization: Bearer …"}
                            rows={3}
                          />
                        </TextField>
                      </div>
                    </Accordion.Body>
                  </Accordion.Panel>
                </Accordion.Item>
              </Accordion>
            </section>
          ))}
        </div>

        <Button variant="secondary" onPress={() => setSources((items) => [...items, newSource()])}>
          <PlusIcon className="size-3.5" /> {t("components.backgroundUpload.addSource")}
        </Button>

        <Accordion
          hideSeparator
          defaultExpandedKeys={["advanced-settings"]}
          className="w-full rounded-xl border border-border"
        >
          <Accordion.Item id="advanced-settings">
            <Accordion.Heading>
              <Accordion.Trigger className="rounded-xl py-3 text-sm font-semibold">
                {t("components.backgroundUpload.advancedSettings")}
                <Accordion.Indicator />
              </Accordion.Trigger>
            </Accordion.Heading>
            <Accordion.Panel>
              <Accordion.Body>
                <div className="grid gap-4 border-border border-t py-4">
                  <div className="grid gap-3 sm:grid-cols-2">
                    <TextField value={minSize} onChange={setMinSize}>
                      <Label>{t("components.backgroundUpload.minSize")}</Label>
                      <Input placeholder={t("components.backgroundUpload.minSizePlaceholder")} />
                    </TextField>
                    <TextField value={maxSize} onChange={setMaxSize}>
                      <Label>{t("components.backgroundUpload.maxSize")}</Label>
                      <Input placeholder={t("components.backgroundUpload.maxSizePlaceholder")} />
                    </TextField>
                    <NumberField
                      aria-label={t("components.backgroundUpload.chunkSizeAria")}
                      value={chunkSizeMiB}
                      minValue={64}
                      maxValue={2000}
                      onChange={(value) =>
                        setChunkSizeMiB(Math.max(64, Math.min(2000, value ?? 512)))
                      }
                    >
                      <Label>{t("components.backgroundUpload.chunkSize")}</Label>
                      <NumberField.Group>
                        <NumberField.DecrementButton />
                        <NumberField.Input />
                        <span className="pr-2 text-xs text-muted">MiB</span>
                        <NumberField.IncrementButton />
                      </NumberField.Group>
                    </NumberField>
                    <NumberField
                      aria-label={t("components.backgroundUpload.concurrentPartsAria")}
                      value={partConcurrency}
                      minValue={1}
                      maxValue={16}
                      onChange={(value) =>
                        setPartConcurrency(Math.max(1, Math.min(16, value ?? 4)))
                      }
                    >
                      <Label>{t("components.backgroundUpload.concurrentParts")}</Label>
                      <NumberField.Group>
                        <NumberField.DecrementButton />
                        <NumberField.Input />
                        <NumberField.IncrementButton />
                      </NumberField.Group>
                    </NumberField>
                  </div>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <TextField>
                      <Label>{t("components.backgroundUpload.batchExclusions")}</Label>
                      <TextArea
                        value={exclude}
                        onChange={(event) => setExclude(event.currentTarget.value)}
                        placeholder={"*.tmp\n**/node_modules/**"}
                        rows={3}
                      />
                    </TextField>
                    <TextField>
                      <Label>{t("components.backgroundUpload.defaultHeaders")}</Label>
                      <TextArea
                        value={headers}
                        onChange={(event) => setHeaders(event.currentTarget.value)}
                        placeholder={"Authorization: Bearer …"}
                        rows={3}
                      />
                    </TextField>
                  </div>
                  <Switch isSelected={encryption} onChange={setEncryption}>
                    <Switch.Content>
                      <Switch.Control>
                        <Switch.Thumb />
                      </Switch.Control>
                      <Label>{t("components.backgroundUpload.encrypt")}</Label>
                    </Switch.Content>
                  </Switch>
                </div>
              </Accordion.Body>
            </Accordion.Panel>
          </Accordion.Item>
        </Accordion>
      </div>
    </AppDialog>
  );
}

/**
 * Splits a textarea value into trimmed, non-empty lines. Returns undefined when
 * nothing is left, which is how the request omits the optional list fields.
 */
function lines(value: string) {
  const result = value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
  return result.length ? result : undefined;
}

/**
 * Parses `Name: value` lines into a header map. A line without a colon, or with
 * an empty name or value, throws a translated error the caller shows in the
 * failure toast. Returns undefined when there is no header to send.
 */
function parseHeaders(value: string, t: Translate) {
  const result: Record<string, string> = {};
  for (const line of lines(value) ?? []) {
    const separator = line.indexOf(":");
    if (separator < 1) throw new Error(t("components.backgroundUpload.invalidHeader", { line }));
    const name = line.slice(0, separator).trim();
    const headerValue = line.slice(separator + 1).trim();
    if (!name || !headerValue)
      throw new Error(t("components.backgroundUpload.invalidHeader", { line }));
    result[name] = headerValue;
  }
  return Object.keys(result).length ? result : undefined;
}

/** Whether the destination text is shaped like a folder UUID rather than a path. */
function isUUID(value: string) {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}
