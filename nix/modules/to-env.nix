{ lib }:
let
  # leafMaps holds the dotted settings paths that must be rendered as one value
  # even though they are attribute sets (encryption.keys), so walk does not
  # descend into them and split them into separate variables.
  leafMaps = import ./generated-leaf-maps.nix;

  # renderScalar renders one setting as the text of a TELDRIVE_ variable.
  # Anything that is not a bool, int or string has no environment spelling, so
  # it aborts the evaluation instead of being silently dropped.
  renderScalar = value:
    if builtins.isBool value then
      lib.boolToString value
    else if builtins.isInt value then
      toString value
    else if builtins.isString value then
      value
    else
      throw "teldrive settings: unsupported value ${builtins.toJSON value}";

  # renderLeaf renders a leaf that is a list or an attribute set in the
  # comma-separated form the Go loader parses back: lists as their elements,
  # attribute sets as key:value entries (encryption.keys).
  renderLeaf = value:
    if builtins.isAttrs value then
      lib.concatStringsSep "," (lib.mapAttrsToList (k: v: "${k}:${renderScalar v}") value)
    else if builtins.isList value then
      lib.concatMapStringsSep "," renderScalar value
    else
      renderScalar value;

  # walk flattens the settings tree into TELDRIVE_* name/value pairs. A null
  # value means "leave the Go default in place" and contributes no variable; a
  # path listed in leafMaps is emitted as one variable instead of being
  # recursed into.
  walk = prefix: attrs:
    lib.concatLists (
      lib.mapAttrsToList (
        name: value:
        let
          path = if prefix == "" then name else "${prefix}.${name}";
        in
        if value == null then
          [ ]
        else if builtins.isAttrs value && !(builtins.elem path leafMaps) then
          walk path value
        else
          [
            {
              name = "TELDRIVE_" + lib.toUpper (lib.replaceStrings [ "-" "." ] [ "_" "_" ] path);
              value = renderLeaf value;
            }
          ]
      ) attrs
    );
in
# settings is a Teldrive settings attribute set from the module options;
# listToAttrs turns the flattened pairs into the environment attribute set the
# NixOS and Home Manager modules install.
settings: builtins.listToAttrs (walk "" settings)
