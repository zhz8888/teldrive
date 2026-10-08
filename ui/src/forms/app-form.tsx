import {
  Button,
  Description,
  FieldError,
  Input,
  Label,
  Switch,
  TextArea,
  TextField,
} from "@heroui/react";
import { createFormHook, createFormHookContexts } from "@tanstack/react-form";
import type { ComponentProps } from "react";
import { useI18n } from "@/lib/i18n";

/**
 * Contexts that tie every field and form component registered at the bottom of
 * this file to the form that rendered it. A field component therefore renders as
 * `field.TextField` inside `form.AppField` without being handed a value: it reads
 * the current field from `useFieldContext`, and a submit control reads its form
 * from `useFormContext`.
 */
const { fieldContext, formContext, useFieldContext, useFormContext } = createFormHookContexts();

/**
 * Collects the messages to show for one field. TanStack Form reports each error
 * either as a string or as a validator result object carrying a `message`, and a
 * field may have several; anything else is dropped. An empty array means the field
 * has nothing to report, which is what keeps the invalid state off.
 */
function fieldErrors(errors: unknown[]) {
  return errors
    .flatMap((error) => {
      if (typeof error === "string") return [error];
      if (
        error &&
        typeof error === "object" &&
        "message" in error &&
        typeof error.message === "string"
      ) {
        return [error.message];
      }
      return [];
    })
    .filter(Boolean);
}

/**
 * State every field component needs: the field from context, its messages, and
 * whether to mark the control invalid. Messages appear only once the field has been
 * touched or a submit has been attempted, so a form does not flag its empty
 * required fields before the user has had a chance to fill them in.
 */
function useFieldPresentation() {
  const field = useFieldContext<unknown>();
  const errors = fieldErrors(field.state.meta.errors);
  const isInvalid =
    (field.state.meta.isTouched || field.form.state.submissionAttempts > 0) && errors.length > 0;

  return {
    field,
    errors,
    isInvalid,
  };
}

/**
 * Presentation props shared by the field components below; everything else comes
 * from the HeroUI control each one wraps.
 */
type CommonFieldProps = {
  /** Caption above the control; the control renders without one when omitted. */
  label?: string;
  /** Helper text under the control, for format hints and consequences. */
  description?: string;
  /** Marks the control as required for assistive technology. */
  isRequired?: boolean;
  /** Disables the control without unmounting it or dropping its value. */
  isDisabled?: boolean;
  /** Asks the control to take focus when it appears, for the first field of a form. */
  autoFocus?: boolean;
};

/** Text input for a string field, with the props the HeroUI `Input` adds to `CommonFieldProps`. */
type AppTextFieldProps = CommonFieldProps &
  Omit<ComponentProps<typeof Input>, "value" | "onChange" | "onBlur" | "isDisabled">;

/**
 * Single-line text field bound to the enclosing `form.AppField` through field
 * context: the field's value fills the input, and typing or leaving it is written
 * back so validators and the submit state see the change. A field holding anything
 * other than a string renders empty, which is how an untouched optional field looks.
 */
function AppTextField({
  label,
  description,
  isRequired,
  isDisabled,
  ...inputProps
}: AppTextFieldProps) {
  const { t } = useI18n();
  const { field, errors, isInvalid } = useFieldPresentation();
  const value = typeof field.state.value === "string" ? field.state.value : "";

  return (
    <TextField
      className="grid gap-1"
      isRequired={isRequired}
      isDisabled={isDisabled}
      isInvalid={isInvalid}
    >
      {label ? <Label>{label}</Label> : null}
      <Input
        {...inputProps}
        value={value}
        onBlur={field.handleBlur}
        onChange={(event) => field.handleChange(event.currentTarget.value)}
      />
      {description ? <Description>{description}</Description> : null}
      {isInvalid ? <FieldError>{errors.join(t("features.form.errorSeparator"))}</FieldError> : null}
    </TextField>
  );
}

/** Multi-line text field, otherwise identical to `AppTextField`. */
type AppTextAreaFieldProps = CommonFieldProps &
  Omit<ComponentProps<typeof TextArea>, "value" | "onChange" | "onBlur" | "isDisabled">;

/**
 * Multi-line text field for a string field (job arguments, tags): same binding and
 * invalid handling as `AppTextField`, with the HeroUI `TextArea` as the control.
 */
function AppTextAreaField({
  label,
  description,
  isRequired,
  isDisabled,
  ...textAreaProps
}: AppTextAreaFieldProps) {
  const { t } = useI18n();
  const { field, errors, isInvalid } = useFieldPresentation();
  const value = typeof field.state.value === "string" ? field.state.value : "";

  return (
    <TextField
      className="grid gap-1"
      isRequired={isRequired}
      isDisabled={isDisabled}
      isInvalid={isInvalid}
    >
      {label ? <Label>{label}</Label> : null}
      <TextArea
        {...textAreaProps}
        value={value}
        onBlur={field.handleBlur}
        onChange={(event) => field.handleChange(event.currentTarget.value)}
      />
      {description ? <Description>{description}</Description> : null}
      {isInvalid ? <FieldError>{errors.join(t("features.form.errorSeparator"))}</FieldError> : null}
    </TextField>
  );
}

/**
 * Boolean field props. `children` is rejected because the switch renders its own
 * label and description inside its content area.
 */
type AppSwitchFieldProps = CommonFieldProps & {
  children?: never;
};

/**
 * Toggle bound to a boolean field: the field's value decides the switch position and
 * flipping it writes straight through, so there is no local copy to keep in sync.
 * Its validation messages are not rendered — a switch has no text to correct — and
 * the accessible name comes from `aria-label` when no visible `label` is given.
 */
function AppSwitchField({
  label,
  description,
  isDisabled,
  "aria-label": ariaLabel,
}: AppSwitchFieldProps & { "aria-label"?: string }) {
  const field = useFieldContext<boolean>();

  return (
    <Switch
      aria-label={ariaLabel}
      isSelected={field.state.value}
      isDisabled={isDisabled}
      onBlur={field.handleBlur}
      onChange={field.handleChange}
    >
      <Switch.Content>
        <Switch.Control>
          <Switch.Thumb />
        </Switch.Control>
        {label ? <Label>{label}</Label> : null}
        {description ? <Description>{description}</Description> : null}
      </Switch.Content>
    </Switch>
  );
}

/**
 * Props of the submit control: everything the HeroUI `Button` takes except the
 * three props the form state owns (`type`, `isPending`, `isDisabled`), so variant
 * and layout props still pass through.
 */
type SubmitButtonProps = Omit<
  ComponentProps<typeof Button>,
  "type" | "isPending" | "isDisabled"
> & {
  /** Keeps the button disabled until a field value has actually changed. */
  requireDirty?: boolean;
};

/**
 * Submit control that takes its state from the form: it submits, shows the pending
 * state while the submit handler runs, and stays disabled while the form cannot be
 * submitted or is already submitting. `requireDirty` is for edit forms where
 * re-submitting unchanged values is pointless.
 */
function SubmitButton({ requireDirty = false, children, ...props }: SubmitButtonProps) {
  const form = useFormContext();

  return (
    <form.Subscribe
      selector={(state) => [state.canSubmit, state.isSubmitting, state.isDirty] as const}
    >
      {([canSubmit, isSubmitting, isDirty]) => (
        <Button
          {...props}
          type="submit"
          isPending={isSubmitting}
          isDisabled={!canSubmit || isSubmitting || (requireDirty && !isDirty)}
        >
          {children}
        </Button>
      )}
    </form.Subscribe>
  );
}

/**
 * The form factory every form in the interface is built from. `useAppForm` creates
 * a form whose `AppField` renders the field components registered here, and
 * `withForm` composes a form a component receives from its parent. Registering the
 * components in this one place is what gives `field.TextField`,
 * `field.TextAreaField`, `field.SwitchField` and `form.SubmitButton` the field and
 * form context, so their values and errors stay wired to the surrounding form.
 */
export const { useAppForm, withForm } = createFormHook({
  fieldComponents: {
    TextField: AppTextField,
    TextAreaField: AppTextAreaField,
    SwitchField: AppSwitchField,
  },
  formComponents: {
    SubmitButton,
  },
  fieldContext,
  formContext,
});
