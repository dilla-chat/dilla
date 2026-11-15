//! The accessibility tree of the automation build (`a11y` command) and the tree lint that replaces beta.5's axe runs
//! (Package 11). Electron exposed Chromium's tree over the ARIA-labelled React UI; the GPUI client exposes AccessKit's,
//! built by GPUI each frame from the elements that have an id and a role. The lint checks what axe checked on the web:
//!
//! - every focusable node has a role and a non-empty name (`label`); a text field may be named by its placeholder;
//! - a focusable control (a button, link, field, tab, switch, menu item, ...) is at least 24 by 24 px (WCAG 2.2
//!   target-size, the rule beta.5's axe ran with), or is inline in text;
//! - a dialog is modal and named;
//! - a node is in the tree once (AccessKit ids are unique; GPUI's debug build also asserts it).
//!
//! Keyboard reachability is checked by walking the tab order ([`crate::Driver::tab_walk`]): every stop must be a node
//! with a role and a name, so a screen reader announces the control and not the whole window.

use std::collections::{HashMap, HashSet};

use serde::Deserialize;

/// One node of the tree (`automation::a11y_json`).
#[derive(Clone, Debug, Deserialize)]
pub struct Node {
    pub id: u64,
    pub role: String,
    pub label: Option<String>,
    pub value: Option<String>,
    pub description: Option<String>,
    pub placeholder: Option<String>,
    #[serde(default)]
    pub children: Vec<u64>,
    /// Logical px, window coordinates: x, y, width, height.
    pub bounds: Option<[f64; 4]>,
    pub click: bool,
    pub focus: bool,
    pub modal: bool,
    pub live: Option<String>,
    pub toggled: Option<String>,
    pub selected: Option<bool>,
    pub expanded: Option<bool>,
    pub disabled: bool,
}

impl Node {
    /// The accessible name: the label, else (a field) its placeholder.
    pub fn name(&self) -> Option<&str> {
        let label = self
            .label
            .as_deref()
            .map(str::trim)
            .filter(|s| !s.is_empty());
        label.or_else(|| {
            matches!(
                self.role.as_str(),
                "TextInput" | "MultilineTextInput" | "SearchInput" | "SpinButton"
            )
            .then(|| {
                self.placeholder
                    .as_deref()
                    .map(str::trim)
                    .filter(|s| !s.is_empty())
            })
            .flatten()
        })
    }

    /// Whether a person can act on it: it takes focus or a click.
    pub fn interactive(&self) -> bool {
        self.focus || self.click
    }
}

/// One frame's tree.
#[derive(Clone, Debug, Deserialize)]
pub struct Tree {
    pub root: Option<u64>,
    pub focus: u64,
    pub nodes: Vec<Node>,
}

/// A rule the tree breaks, with the node it is about.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Violation {
    pub rule: &'static str,
    pub node: String,
}

impl std::fmt::Display for Violation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.rule, self.node)
    }
}

/// The minimum target, logical px (WCAG 2.2 SC 2.5.8, axe `target-size`).
pub const TARGET_MIN: f64 = 24.0;

/// Roles a person aims a pointer at, where axe's `target-size` applies.
const TARGET_ROLES: &[&str] = &[
    "Button",
    "Link",
    "CheckBox",
    "RadioButton",
    "Switch",
    "Tab",
    "MenuItem",
    "MenuItemCheckBox",
    "MenuItemRadio",
    "ComboBox",
    "ListBoxOption",
    "Slider",
];

impl Tree {
    /// Parses the `a11y` reply's JSON.
    pub fn parse(json: &str) -> Result<Self, serde_json::Error> {
        serde_json::from_str(json)
    }

    pub fn node(&self, id: u64) -> Option<&Node> {
        self.nodes.iter().find(|n| n.id == id)
    }

    /// The node that has the focus, `None` when it is the window (no node holds it, or the focused element has no node).
    pub fn focused(&self) -> Option<&Node> {
        if Some(self.focus) == self.root {
            return None;
        }
        self.node(self.focus)
    }

    /// Every node with this role.
    pub fn by_role<'a>(&'a self, role: &'a str) -> impl Iterator<Item = &'a Node> + 'a {
        self.nodes.iter().filter(move |n| n.role == role)
    }

    /// The first node with this role and name.
    pub fn find(&self, role: &str, name: &str) -> Option<&Node> {
        self.nodes
            .iter()
            .find(|n| n.role == role && n.name() == Some(name))
    }

    /// Whether a node with this role and name is in the tree.
    pub fn has(&self, role: &str, name: &str) -> bool {
        self.find(role, name).is_some()
    }

    /// The parent of every node.
    fn parents(&self) -> HashMap<u64, u64> {
        let mut parents = HashMap::new();
        for n in &self.nodes {
            for c in &n.children {
                parents.insert(*c, n.id);
            }
        }
        parents
    }

    /// The ancestors of `id`, nearest first.
    pub fn ancestors(&self, id: u64) -> Vec<&Node> {
        let parents = self.parents();
        let mut out = Vec::new();
        let mut at = id;
        while let Some(p) = parents.get(&at) {
            if let Some(n) = self.node(*p) {
                out.push(n);
            }
            at = *p;
        }
        out
    }

    /// Whether `id` lies inside a node with this role (and, given, this name).
    pub fn inside(&self, id: u64, role: &str, name: Option<&str>) -> bool {
        self.ancestors(id)
            .iter()
            .any(|a| a.role == role && name.is_none_or(|name| a.name() == Some(name)))
    }

    /// A short description of a node for a failure message.
    pub fn describe(&self, n: &Node) -> String {
        let path: Vec<String> = self
            .ancestors(n.id)
            .iter()
            .rev()
            .filter(|a| a.name().is_some())
            .map(|a| format!("{}({})", a.role, a.name().unwrap_or_default()))
            .collect();
        format!(
            "{} {:?} value={:?} bounds={:?} in [{}]",
            n.role,
            n.name(),
            n.value,
            n.bounds,
            path.join(" > ")
        )
    }

    /// A pointer target (a control role a person can act on) with a visible box.
    fn target(n: &Node) -> Option<[f64; 4]> {
        let b = n.bounds?;
        (n.interactive() && TARGET_ROLES.contains(&n.role.as_str()) && b[2] > 0.0 && b[3] > 0.0)
            .then_some(b)
    }

    /// A target smaller than 24 by 24.
    fn undersized(&self, n: &Node) -> bool {
        Self::target(n).is_some_and(|[_, _, w, h]| w + 0.01 < TARGET_MIN || h + 0.01 < TARGET_MIN)
    }

    /// SC 2.5.8's spacing exception (axe `target-size`): a 24 px circle centred on the undersized target `n` meets no
    /// other target, nor the circle of another undersized one. Targets that contain one another (a button in a
    /// clickable row) are not neighbours.
    fn spaced(&self, n: &Node) -> bool {
        let Some(a) = Self::target(n) else {
            return true;
        };
        let centre = |b: [f64; 4]| (b[0] + b[2] / 2.0, b[1] + b[3] / 2.0);
        let (cx, cy) = centre(a);
        let r = TARGET_MIN / 2.0;
        let related: HashSet<u64> = self
            .ancestors(n.id)
            .iter()
            .map(|a| a.id)
            .chain(self.descendants(n.id))
            .collect();
        self.nodes.iter().all(|m| {
            if m.id == n.id || related.contains(&m.id) {
                return true;
            }
            let Some(b) = Self::target(m) else {
                return true;
            };
            if self.undersized(m) {
                let (mx, my) = centre(b);
                ((mx - cx).powi(2) + (my - cy).powi(2)).sqrt() + 0.01 >= TARGET_MIN
            } else {
                let dx = (b[0] - cx).max(0.0).max(cx - (b[0] + b[2]));
                let dy = (b[1] - cy).max(0.0).max(cy - (b[1] + b[3]));
                (dx * dx + dy * dy).sqrt() + 0.01 >= r
            }
        })
    }

    /// Every node under `id`.
    fn descendants(&self, id: u64) -> Vec<u64> {
        let mut out = Vec::new();
        let mut stack = vec![id];
        while let Some(at) = stack.pop() {
            if let Some(n) = self.node(at) {
                for c in &n.children {
                    out.push(*c);
                    stack.push(*c);
                }
            }
        }
        out
    }

    /// The tree lint (module doc). Empty when the tree passes.
    pub fn lint(&self) -> Vec<Violation> {
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        for n in &self.nodes {
            if !seen.insert(n.id) {
                out.push(Violation {
                    rule: "duplicate-id",
                    node: self.describe(n),
                });
            }
            if Some(n.id) == self.root {
                continue;
            }
            if n.interactive() && n.name().is_none() {
                out.push(Violation {
                    rule: "focusable-without-name",
                    node: self.describe(n),
                });
            }
            if self.undersized(n) && !self.spaced(n) {
                out.push(Violation {
                    rule: "target-size",
                    node: self.describe(n),
                });
            }
            if matches!(n.role.as_str(), "Dialog" | "AlertDialog") {
                if !n.modal {
                    out.push(Violation {
                        rule: "dialog-not-modal",
                        node: self.describe(n),
                    });
                }
                if n.name().is_none() {
                    out.push(Violation {
                        rule: "dialog-without-name",
                        node: self.describe(n),
                    });
                }
            }
        }
        out
    }
}
