import { AlertDialog, Button } from "@heroui/react";
import { useI18n } from "@/lib/i18n";
import TrashBinIcon from "~icons/gravity-ui/trash-bin";

interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
  title: string;
  message: string;
  confirmLabel?: string;
  isPending?: boolean;
}

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
