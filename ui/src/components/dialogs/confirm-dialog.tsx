import { AlertDialog, Button } from "@heroui/react";
import { useI18n } from "@/lib/i18n";
import TrashBinIcon from "~icons/gravity-ui/trash-bin";

/**
 * Props of {@link ConfirmDialog}. The dialog only reports the confirmation:
 * `onConfirm` runs and closing the dialog stays the caller's job, which is what
 * keeps it open while the action is still in flight.
 */
interface ConfirmDialogProps {
  /** Whether the alert dialog is shown. */
  open: boolean;
  /** Receives the requested visibility; the cancel button closes through it. */
  onOpenChange: (open: boolean) => void;
  /** Runs when the danger button is pressed; it does not close the dialog. */
  onConfirm: () => void;
  /** Heading naming the action, usually the item it applies to. */
  title: string;
  /** Body sentence describing what the action does. */
  message: string;
  /** Label of the danger button; defaults to the shared "delete" action. */
  confirmLabel?: string;
  /** Keeps the danger button pending while the action runs; defaults to false. */
  isPending?: boolean;
}

/**
 * Alert dialog for a destructive action: fixed danger icon, a cancel button that
 * closes the dialog itself, and a confirm button that only calls back.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  onConfirm,
  title,
  message,
  confirmLabel,
  isPending = false,
}: ConfirmDialogProps) {
  const { t } = useI18n();

  return (
    <AlertDialog.Backdrop isOpen={open} onOpenChange={onOpenChange}>
      <AlertDialog.Container>
        <AlertDialog.Dialog className="sm:max-w-[400px]">
          <AlertDialog.CloseTrigger />
          <AlertDialog.Header>
            <AlertDialog.Icon status="danger">
              <TrashBinIcon className="size-5" />
            </AlertDialog.Icon>
            <AlertDialog.Heading>{title}</AlertDialog.Heading>
          </AlertDialog.Header>
          <AlertDialog.Body>
            <p>{message}</p>
          </AlertDialog.Body>
          <AlertDialog.Footer>
            <Button slot="close" variant="tertiary">
              {t("common.action.cancel")}
            </Button>
            <Button
              variant="danger"
              isPending={isPending}
              onPress={() => {
                onConfirm();
              }}
            >
              {confirmLabel ?? t("common.action.delete")}
            </Button>
          </AlertDialog.Footer>
        </AlertDialog.Dialog>
      </AlertDialog.Container>
    </AlertDialog.Backdrop>
  );
}
