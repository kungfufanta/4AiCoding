// Styled Radix Select (accessible, keyboard-navigable). Used for role
// pickers; Radix positions its content with inline style attributes,
// which is why webfs CSP carries style-src 'unsafe-inline' (P1).

import * as SelectPrimitive from "@radix-ui/react-select";
import { Check, ChevronDown } from "lucide-react";
import { cn } from "../lib/cn";

export type SelectOption = { value: string; label: string };

export function Select({
  value,
  onValueChange,
  options,
  placeholder,
  ariaLabel,
  disabled,
  className,
}: {
  value: string;
  onValueChange: (value: string) => void;
  options: SelectOption[];
  placeholder?: string;
  ariaLabel: string;
  disabled?: boolean;
  className?: string;
}) {
  return (
    <SelectPrimitive.Root value={value} onValueChange={onValueChange} disabled={disabled}>
      <SelectPrimitive.Trigger
        aria-label={ariaLabel}
        className={cn(
          "inline-flex h-9 min-w-32 items-center justify-between gap-2 rounded-[var(--radius)] border border-border bg-bg px-3 text-sm text-fg",
          "data-[placeholder]:text-muted disabled:opacity-60",
          className,
        )}
      >
        <SelectPrimitive.Value placeholder={placeholder} />
        <SelectPrimitive.Icon>
          <ChevronDown aria-hidden className="size-4 text-muted" />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          position="popper"
          sideOffset={4}
          className="z-50 max-h-64 min-w-[var(--radix-select-trigger-width)] overflow-hidden rounded-[var(--radius)] border border-border bg-surface shadow-lg"
        >
          <SelectPrimitive.Viewport className="p-1">
            {options.map((o) => (
              <SelectPrimitive.Item
                key={o.value}
                value={o.value}
                className="flex cursor-pointer items-center justify-between gap-2 rounded-md px-2.5 py-1.5 text-sm text-fg outline-none data-[highlighted]:bg-surface-2 data-[disabled]:opacity-50"
              >
                <SelectPrimitive.ItemText>{o.label}</SelectPrimitive.ItemText>
                <SelectPrimitive.ItemIndicator>
                  <Check aria-hidden className="size-4 text-brand" />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}
