//! Task 26's components (`components/factor/`): the six code boxes, the backup-code list and a sign-in factor's row
//! (packages/ui `CodeBoxes`, `CodeList`, `FactorRow`). They draw only; the screens own the state.

pub mod code_boxes;
pub mod code_list;
pub mod factor_row;

pub use code_boxes::{CODE_LENGTH, COMPLETE_DELAY, CodeBoxes, code_digits};
pub use code_list::{CodeList, columns as code_list_columns, copy_text, group_code};
pub use factor_row::{FactorRow, Place};
