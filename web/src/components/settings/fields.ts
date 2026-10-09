// The settings field table, generically (issue #61, issue #85): a one-page
// settings form is described by ONE entry per PATCHable key, and that table is
// the page's single source for everything the one save rule needs to know
// about a field —
//
//   - which section it lives in (the save bar's section links, the chips' and
//     outline's "unsaved changes" marks, the `?field=` lookup),
//   - its label,
//   - how its draft seeds from the saved object,
//   - how the draft normalises to the wire,
//   - the rule the browser checks before Save sends anything,
//   - and whether the field is OVERRIDABLE: an entry with an `inherit` draft
//     is a field whose own value may be null, meaning "inherit" — the page
//     then shows "inherited" or "set here" at its label, and Reset puts the
//     `inherit` draft back (saved as a null override).
//
// A page names its table's types once, as a FormShape (its drafts, its PATCH
// body, its saved object, what a wire form may read besides its own draft,
// its form section slugs), writes the entries IN PAGE ORDER — so "the first
// problem" and the order of the changed sections fall out of the table — and
// hands them to createFieldTable(), which derives every rule below from them.
// The repo page's table is routes/repo-settings/fields.ts.
//
// Every function here is pure. A draft is always a control's own value type
// (a string for text, number and select fields, a boolean for a switch, an
// object for a composite control); the wire value is what the page's PATCH
// takes for that key.

/** The types one settings form is built over. */
export interface FormShape {
  /** The draft type of every field, keyed by its PATCH key. */
  drafts: object;
  /** The PATCH body: one (optional) key per field. */
  patch: object;
  /** What the drafts seed from and are diffed against (a repo, the settings). */
  saved: unknown;
  /** What a field's wire value may depend on besides its own draft. */
  context: unknown;
  /** The slugs of the sections whose fields wait for Save. */
  section: string;
}

/** A field's key: a key of both the drafts and the PATCH. */
export type FieldKey<T extends FormShape> = keyof T['drafts'] & keyof T['patch'] & string;

/** The draft type of one field. */
export type DraftOf<T extends FormShape, K extends FieldKey<T>> = T['drafts'][K];

export interface FieldSpec<T extends FormShape, K extends FieldKey<T>> {
  key: K;
  section: T['section'];
  /** The label the page prints at the field. */
  label: string;
  /** The draft a saved object shows. */
  seed: (saved: T['saved']) => DraftOf<T, K>;
  /** Draft equality, for object-valued drafts; default `===`. */
  same?: (a: DraftOf<T, K>, b: DraftOf<T, K>) => boolean;
  /** The draft as the PATCH takes it. */
  wire: (draft: DraftOf<T, K>, context: T['context']) => T['patch'][K];
  /** Whether a draft differs from the saved one; default: their wire values differ. */
  differs?: (draft: DraftOf<T, K>, saved: DraftOf<T, K>, context: T['context']) => boolean;
  /** What is wrong with a draft, in the operator's words; null = nothing. */
  validate?: (draft: DraftOf<T, K>) => string | null;
  /**
   * Present exactly on the OVERRIDABLE fields: the draft that means "inherit"
   * (null on the wire). Reset sets it.
   */
  inherit?: DraftOf<T, K>;
}

/** A whole table: one entry per field, in page order. */
export type FieldSpecs<T extends FormShape> = { [K in FieldKey<T>]: FieldSpec<T, K> };

/** The operator's edits: a draft per touched field, absent for an untouched one. */
export type FieldEdits<T extends FormShape> = { [K in FieldKey<T>]?: DraftOf<T, K> };

/** Problems by field, in the operator's words. */
export type FieldProblems<T extends FormShape> = Partial<Record<FieldKey<T>, string>>;

/** Everything the one save rule derives from a table. */
export interface FieldTable<T extends FormShape> {
  /** The entries, as written. */
  fields: FieldSpecs<T>;
  /** Every field key, in page order. */
  keys: readonly FieldKey<T>[];
  /** The table entry of one field. */
  spec<K extends FieldKey<T>>(key: K): FieldSpec<T, K>;
  /** Narrows an arbitrary string (a `?field=` value, a refusal's field) to a field key. */
  isKey(value: string | undefined | null): value is FieldKey<T>;
  /** Whether a field's own value may be null, meaning "inherit". */
  isOverridable(key: FieldKey<T>): boolean;
  /** The overridable fields, in page order. */
  overridableKeys: readonly FieldKey<T>[];
  /** Draft equality under the field's own rule. */
  sameDraft<K extends FieldKey<T>>(key: K, a: DraftOf<T, K>, b: DraftOf<T, K>): boolean;
  /** Whether `draft` would change the saved object if it were sent. */
  draftDiffers<K extends FieldKey<T>>(
    key: K,
    draft: DraftOf<T, K>,
    saved: T['saved'],
    context: T['context'],
  ): boolean;
  /**
   * Whether a draft of an overridable field leaves it inherited: it would be
   * saved as null. Always false for a field that cannot inherit.
   */
  draftInherits<K extends FieldKey<T>>(
    key: K,
    draft: DraftOf<T, K>,
    context: T['context'],
  ): boolean;
  /** One draft's problem under its field's rule, or null. */
  validateDraft<K extends FieldKey<T>>(key: K, draft: DraftOf<T, K>): string | null;
  /** The edited fields that differ from the saved object, in page order. */
  changedFields(edits: FieldEdits<T>, saved: T['saved'], context: T['context']): FieldKey<T>[];
  /**
   * The one PATCH of a Save: exactly the changed fields, each in its wire
   * form. Diffed against the SAVED object, so a field the operator never
   * touched can never be sent — and a server-side change to it never reverted.
   */
  buildPatch(edits: FieldEdits<T>, saved: T['saved'], context: T['context']): T['patch'];
  /**
   * The problems of the CHANGED fields under each one's own rule (an
   * untouched field is never sent, so never checked). A page adds the checks
   * that span fields on top.
   */
  validateEdits(edits: FieldEdits<T>, saved: T['saved'], context: T['context']): FieldProblems<T>;
  /** The sections that hold `keys`, in the order of `keys`. */
  sectionsOf(keys: readonly FieldKey<T>[]): T['section'][];
}

/** Builds the derived rules of a table whose entries are in page order. */
export function createFieldTable<T extends FormShape>(fields: FieldSpecs<T>): FieldTable<T> {
  type Key = FieldKey<T>;
  const keys = Object.keys(fields) as Key[];
  const spec = <K extends Key>(key: K): FieldSpec<T, K> => fields[key];
  const isOverridable = (key: Key): boolean => Object.hasOwn(fields[key], 'inherit');
  const sameDraft = <K extends Key>(key: K, a: DraftOf<T, K>, b: DraftOf<T, K>): boolean => {
    const entry = spec(key);
    return entry.same !== undefined ? entry.same(a, b) : a === b;
  };
  const draftDiffers = <K extends Key>(
    key: K,
    draft: DraftOf<T, K>,
    saved: T['saved'],
    context: T['context'],
  ): boolean => {
    const entry = spec(key);
    const base = entry.seed(saved);
    return entry.differs !== undefined
      ? entry.differs(draft, base, context)
      : entry.wire(draft, context) !== entry.wire(base, context);
  };
  const isChanged = <K extends Key>(
    key: K,
    edits: FieldEdits<T>,
    saved: T['saved'],
    context: T['context'],
  ): boolean => {
    const draft = edits[key];
    return draft !== undefined && draftDiffers(key, draft as DraftOf<T, K>, saved, context);
  };
  const changedFields = (edits: FieldEdits<T>, saved: T['saved'], context: T['context']): Key[] =>
    keys.filter((key) => isChanged(key, edits, saved, context));
  const validateDraft = <K extends Key>(key: K, draft: DraftOf<T, K>): string | null =>
    spec(key).validate?.(draft) ?? null;

  return {
    fields,
    keys,
    spec,
    isKey: (value): value is Key => typeof value === 'string' && Object.hasOwn(fields, value),
    isOverridable,
    overridableKeys: keys.filter(isOverridable),
    sameDraft,
    draftDiffers,
    draftInherits: (key, draft, context) =>
      isOverridable(key) && spec(key).wire(draft, context) === null,
    validateDraft,
    changedFields,
    buildPatch: (edits, saved, context) => {
      const patch: Record<string, unknown> = {};
      for (const key of keys) {
        if (isChanged(key, edits, saved, context)) {
          patch[key] = spec(key).wire(edits[key] as DraftOf<T, typeof key>, context);
        }
      }
      return patch as T['patch'];
    },
    validateEdits: (edits, saved, context) => {
      const problems: FieldProblems<T> = {};
      for (const key of changedFields(edits, saved, context)) {
        const message = validateDraft(key, edits[key] as DraftOf<T, typeof key>);
        if (message !== null) problems[key] = message;
      }
      return problems;
    },
    sectionsOf: (of) => {
      const sections: T['section'][] = [];
      for (const key of of) {
        const section = spec(key).section;
        if (!sections.includes(section)) sections.push(section);
      }
      return sections;
    },
  };
}
