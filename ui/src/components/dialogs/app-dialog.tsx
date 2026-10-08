import { cn, Modal } from "@heroui/react";
import type { ReactNode } from "react";

/** Default surface: the fixed width applies from `sm` up, 92vw capped at 30rem. */
const DEFAULT_DIALOG_CLASS = "min-w-0 sm:w-[min(92vw,30rem)] sm:max-w-none bg-surface";

/**
 * Props of {@link AppDialog}. The dialog is controlled: it reports the
 * visibility it wants, and the parent decides whether to change it.
 */
type AppDialogProps = {
  /** Whether the dialog is rendered; the parent owns this state. */
  open: boolean;
  /** Receives the visibility the dialog asks for on dismiss. */
  onOpenChange: (open: boolean) => void;
  /** Heading shown at the top of the modal. */
  title: ReactNode;
  /** Optional smaller line under the heading. */
  description?: ReactNode;
  /** Body content, scrolled inside the modal when it overflows. */
  children: ReactNode;
  /** Optional action row; no footer element is rendered when it is absent. */
  footer?: ReactNode;
  /** Whether pressing the backdrop closes the dialog; defaults to true. */
  isDismissable?: boolean;
  /** Disables the close trigger and Escape dismissal; defaults to false. */
  isCloseDisabled?: boolean;
  /** Width preset handed to the modal container; defaults to "lg". */
  size?: "md" | "lg";
  /** Classes merged after the default width, so they can override it. */
  className?: string;
  /** Classes for the scrolling body element. */
  bodyClassName?: string;
  /** Classes for the header element. */
  headerClassName?: string;
};

/**
 * Modal shell shared by the app's dialogs: heading, optional description,
 * scrolling body and optional footer, on the common surface. Dismissal is
 * routed through `onOpenChange`, so the caller keeps full control.
 */
export function AppDialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  isDismissable = true,
  isCloseDisabled = false,
  size = "lg",
  className,
  bodyClassName,
  headerClassName,
}: AppDialogProps) {
  return (
    <Modal.Backdrop
      isOpen={open}
      onOpenChange={onOpenChange}
      isDismissable={isDismissable}
      isKeyboardDismissDisabled={isCloseDisabled}
    >
      <Modal.Container size={size} scroll="inside">
        <Modal.Dialog className={cn(DEFAULT_DIALOG_CLASS, className)}>
          <Modal.CloseTrigger isDisabled={isCloseDisabled} />
          <Modal.Header className={headerClassName}>
            <Modal.Heading>{title}</Modal.Heading>
            {description ? <div className="text-sm text-muted">{description}</div> : null}
          </Modal.Header>
          <Modal.Body className={bodyClassName}>{children}</Modal.Body>
          {footer ? <Modal.Footer>{footer}</Modal.Footer> : null}
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  );
}
