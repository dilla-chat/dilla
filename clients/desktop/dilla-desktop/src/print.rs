//! Printing, the desktop's `window.print()` (Onboarding.tsx's backup codes, Jonas 2026-11-14: Print, like the web).
//!
//! GPUI has no print API. On Linux the app hands the system's print dialog a document through the XDG desktop portal
//! (`org.freedesktop.portal.Print`, which GNOME, KDE and the other desktops implement with their own dialog): a
//! `PreparePrint` shows the dialog, and a `Print` with the token it answered passes the document as a file descriptor.
//!
//! The document is a small PDF the app writes itself ([`codes_pdf`]: the portal's backends print PDF; plain text is not
//! promised). It never touches the disk: it lives in an anonymous memory file (`memfd_create`) sealed against changes,
//! which the kernel frees when the last descriptor closes (the app's when the print request has answered, the
//! backend's when it has read the job). Every buffer of it on the app's side is a [`Zeroizing`] one. What the printing
//! system then does with the job (a spool file, the printer) is the system's, as for the web's print.
//!
//! Other platforms have no print path yet: [`system`] answers [`PrintError::Unsupported`].

use std::fmt::Write as _;

use zeroize::Zeroizing;

/// What a print request came to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Printed {
    /// The dialog took the document.
    Sent,
    /// The person closed the dialog without printing.
    Cancelled,
}

/// Why nothing was printed.
#[derive(Debug)]
pub enum PrintError {
    /// This platform has no print path (or no print portal runs).
    Unsupported,
    /// The document could not be handed over.
    Failed(String),
}

impl PrintError {
    /// The code a refusal shows (`factor.error.other`'s `{code}`).
    pub fn code(&self) -> &'static str {
        match self {
            Self::Unsupported => "E_PRINT_UNSUPPORTED",
            Self::Failed(_) => "E_PRINT_FAILED",
        }
    }
}

/// A4 in PostScript points, 56pt (about 2 cm) margins.
const PAGE_W: f32 = 595.0;
const PAGE_H: f32 = 842.0;
const MARGIN: f32 = 56.0;
/// The title, the copy and the codes: their sizes and line advances, in points.
const TITLE_PT: f32 = 16.0;
const BODY_PT: f32 = 11.0;
const CODE_PT: f32 = 12.0;
const LEAD: f32 = 1.45;
/// The copy is wrapped at this many characters (Helvetica at 11pt across the text width, conservatively).
const WRAP: usize = 80;

/// A PDF of the backup codes as the web prints them (CodeList.tsx under `@media print`): the title, the copy, the list's
/// label and the codes, numbered, one per line, in a monospaced face. Every buffer it is built in is zeroized.
pub fn codes_pdf(title: &str, body: &str, label: &str, codes: &[String]) -> Zeroizing<Vec<u8>> {
    let mut text = Zeroizing::new(String::new());
    let mut y = PAGE_H - MARGIN - TITLE_PT;
    let put = |text: &mut String, font: &str, size: f32, y: f32, line: &str| {
        let _ = writeln!(
            text,
            "BT /{font} {size} Tf {MARGIN} {y:.2} Td ({}) Tj ET",
            pdf_string(line).as_str()
        );
    };
    put(&mut text, "F2", TITLE_PT, y, title);
    y -= TITLE_PT * LEAD * 1.5;
    for line in wrap(body, WRAP) {
        put(&mut text, "F1", BODY_PT, y, &line);
        y -= BODY_PT * LEAD;
    }
    y -= BODY_PT * LEAD;
    put(&mut text, "F2", BODY_PT, y, label);
    y -= CODE_PT * LEAD * 1.25;
    for (i, code) in codes.iter().enumerate() {
        let numbered = Zeroizing::new(format!("{:>2}. {}", i + 1, code));
        put(&mut text, "F3", CODE_PT, y, &numbered);
        y -= CODE_PT * LEAD;
    }
    document(text.as_bytes())
}

/// Lays one content stream out as a one-page PDF: the catalog, the page tree, the page, its content and the three
/// standard faces (Helvetica, Helvetica-Bold, Courier; WinAnsi), then the cross-reference table and the trailer.
fn document(content: &[u8]) -> Zeroizing<Vec<u8>> {
    let mut out = Zeroizing::new(Vec::with_capacity(content.len() + 1024));
    let mut offsets = Vec::with_capacity(7);
    out.extend_from_slice(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n");
    let font = |name: &str| {
        format!("<< /Type /Font /Subtype /Type1 /BaseFont /{name} /Encoding /WinAnsiEncoding >>")
    };
    let objects: [Vec<u8>; 3] = [
        b"<< /Type /Catalog /Pages 2 0 R >>".to_vec(),
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>".to_vec(),
        format!(
            "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 {PAGE_W} {PAGE_H}] \
             /Resources << /Font << /F1 5 0 R /F2 6 0 R /F3 7 0 R >> >> /Contents 4 0 R >>"
        )
        .into_bytes(),
    ];
    for (i, body) in objects.iter().enumerate() {
        offsets.push(out.len());
        out.extend_from_slice(format!("{} 0 obj\n", i + 1).as_bytes());
        out.extend_from_slice(body);
        out.extend_from_slice(b"\nendobj\n");
    }
    offsets.push(out.len());
    out.extend_from_slice(format!("4 0 obj\n<< /Length {} >>\nstream\n", content.len()).as_bytes());
    out.extend_from_slice(content);
    out.extend_from_slice(b"\nendstream\nendobj\n");
    for (n, name) in [(5, "Helvetica"), (6, "Helvetica-Bold"), (7, "Courier")] {
        offsets.push(out.len());
        out.extend_from_slice(format!("{n} 0 obj\n{}\nendobj\n", font(name)).as_bytes());
    }
    let xref = out.len();
    out.extend_from_slice(
        format!("xref\n0 {}\n0000000000 65535 f \n", offsets.len() + 1).as_bytes(),
    );
    for at in &offsets {
        out.extend_from_slice(format!("{at:010} 00000 n \n").as_bytes());
    }
    out.extend_from_slice(
        format!(
            "trailer\n<< /Size {} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n",
            offsets.len() + 1
        )
        .as_bytes(),
    );
    out
}

/// A PDF literal string (WinAnsi): `\`, `(` and `)` escaped, the typographic quotes and dashes the copy uses in their
/// WinAnsi codes, anything else outside it as `?`.
fn pdf_string(s: &str) -> Zeroizing<String> {
    let mut out = Zeroizing::new(String::with_capacity(s.len() + 8));
    for c in s.chars() {
        let byte: Option<u8> = match c {
            '\\' | '(' | ')' => {
                out.push('\\');
                out.push(c);
                continue;
            }
            ' '..='~' => {
                out.push(c);
                continue;
            }
            '‘' => Some(0x91),
            '’' => Some(0x92),
            '“' => Some(0x93),
            '”' => Some(0x94),
            '–' => Some(0x96),
            '—' => Some(0x97),
            '…' => Some(0x85),
            c if ('\u{a0}'..='\u{ff}').contains(&c) => u8::try_from(u32::from(c)).ok(),
            _ => None,
        };
        match byte {
            Some(b) => {
                let _ = write!(out, "\\{b:03o}");
            }
            None => out.push('?'),
        }
    }
    out
}

/// Words joined into lines of at most `width` characters (a longer word stands alone).
fn wrap(s: &str, width: usize) -> Vec<String> {
    let mut lines = Vec::new();
    let mut line = String::new();
    for word in s.split_whitespace() {
        if !line.is_empty() && line.chars().count() + 1 + word.chars().count() > width {
            lines.push(std::mem::take(&mut line));
        }
        if !line.is_empty() {
            line.push(' ');
        }
        line.push_str(word);
    }
    if !line.is_empty() {
        lines.push(line);
    }
    lines
}

/// Prints `pdf` through the system's print dialog, titled `title` (see the module's comment). Linux only for now.
#[cfg(target_os = "linux")]
pub async fn system(title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    use std::io::{Seek as _, SeekFrom, Write as _};
    use std::os::fd::AsFd as _;

    use ashpd::desktop::ResponseError;
    use ashpd::desktop::print::{PreparePrintOptions, PrintOptions, PrintProxy};
    use rustix::fs::{MemfdFlags, SealFlags};

    let failed = |e: &dyn std::fmt::Display| PrintError::Failed(e.to_string());
    let fd = rustix::fs::memfd_create(
        "dilla-print",
        MemfdFlags::CLOEXEC | MemfdFlags::ALLOW_SEALING,
    )
    .map_err(|e| failed(&e))?;
    let mut file = std::fs::File::from(fd);
    file.write_all(&pdf).map_err(|e| failed(&e))?;
    drop(pdf);
    file.seek(SeekFrom::Start(0)).map_err(|e| failed(&e))?;
    // The backend reads what the app wrote, and nothing can change it after.
    rustix::fs::fcntl_add_seals(
        &file,
        SealFlags::SEAL | SealFlags::SHRINK | SealFlags::GROW | SealFlags::WRITE,
    )
    .map_err(|e| failed(&e))?;
    let proxy = PrintProxy::new()
        .await
        .map_err(|_| PrintError::Unsupported)?;
    let prepared = proxy
        .prepare_print(
            None,
            &title,
            Default::default(),
            Default::default(),
            PreparePrintOptions::default().set_modal(true),
        )
        .await
        .map_err(|e| failed(&e))?
        .response();
    let prepared = match prepared {
        Ok(p) => p,
        Err(ashpd::Error::Response(ResponseError::Cancelled)) => return Ok(Printed::Cancelled),
        Err(e) => return Err(failed(&e)),
    };
    let sent = proxy
        .print(
            None,
            &title,
            &file.as_fd(),
            PrintOptions::default()
                .set_token(prepared.token)
                .set_modal(true),
        )
        .await
        .map_err(|e| failed(&e))?
        .response();
    // The app's descriptor closes here; the memory file goes when the backend has closed its own.
    drop(file);
    match sent {
        Ok(()) => Ok(Printed::Sent),
        Err(ashpd::Error::Response(ResponseError::Cancelled)) => Ok(Printed::Cancelled),
        Err(e) => Err(failed(&e)),
    }
}

/// Prints `pdf` through the system's print dialog: no print path on this platform yet.
#[cfg(not(target_os = "linux"))]
#[allow(clippy::unused_async)]
pub async fn system(_title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    drop(pdf);
    Err(PrintError::Unsupported)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn codes() -> Vec<String> {
        (0..10)
            .map(|i| format!("ABCDE-FGHIJ-KLMNO-P{i:04}"))
            .collect()
    }

    fn pdf() -> Zeroizing<Vec<u8>> {
        codes_pdf(
            "Your backup codes",
            "Each backup code signs you in once if you lose your second factor. Keep them with your recovery key.",
            "Backup codes",
            &codes(),
        )
    }

    /// The document is a PDF whose cross-reference table points at each object it lists, and whose `startxref` points
    /// at that table: what a print backend's parser checks first.
    #[test]
    fn the_pdf_is_well_formed() {
        let bytes = pdf();
        assert!(bytes.starts_with(b"%PDF-1.4\n"));
        assert!(bytes.ends_with(b"%%EOF\n"));
        // Byte offsets: the binary comment's bytes stand in as `?`, one byte each.
        let text = String::from_utf8(
            bytes
                .iter()
                .map(|&b| if b.is_ascii() { b } else { b'?' })
                .collect(),
        )
        .unwrap();
        let start: usize = text
            .rsplit("startxref\n")
            .next()
            .and_then(|t| t.lines().next())
            .and_then(|n| n.parse().ok())
            .expect("startxref");
        assert!(text[start..].starts_with("xref\n0 8\n"));
        let table: Vec<usize> = text[start..]
            .lines()
            .skip(3)
            .take(7)
            .map(|l| l[..10].parse().unwrap())
            .collect();
        for (i, at) in table.iter().enumerate() {
            assert!(
                text[*at..].starts_with(&format!("{} 0 obj\n", i + 1)),
                "object {} at {at}",
                i + 1
            );
        }
        let length: usize = text
            .split("/Length ")
            .nth(1)
            .and_then(|t| t.split(' ').next())
            .and_then(|n| n.parse().ok())
            .unwrap();
        let stream = text.find("stream\n").unwrap() + "stream\n".len();
        assert_eq!(&text[stream + length..stream + length + 10], "\nendstream");
    }

    /// Every code is in the document once, numbered, in the monospaced face; the title and the copy are there.
    #[test]
    fn the_pdf_holds_the_title_the_copy_and_every_code() {
        let text = String::from_utf8_lossy(&pdf()).into_owned();
        assert!(text.contains("(Your backup codes) Tj"));
        assert!(text.contains("(Each backup code signs you in once"));
        assert!(text.contains("/BaseFont /Courier "));
        for (i, code) in codes().iter().enumerate() {
            assert_eq!(
                text.matches(&format!("({:>2}. {code}) Tj", i + 1)).count(),
                1,
                "{code}"
            );
        }
    }

    #[test]
    fn strings_are_escaped_for_pdf() {
        assert_eq!(pdf_string(r"a(b)c\d").as_str(), r"a\(b\)c\\d");
        assert_eq!(pdf_string("You’re in").as_str(), r"You\222re in");
        assert_eq!(pdf_string("é").as_str(), r"\351");
        assert_eq!(pdf_string("⇧").as_str(), "?");
    }

    #[test]
    fn the_copy_wraps_between_words() {
        let lines = wrap("one two three four", 9);
        assert_eq!(lines, ["one two", "three", "four"]);
    }

    #[test]
    fn a_refusal_has_a_code() {
        assert_eq!(PrintError::Unsupported.code(), "E_PRINT_UNSUPPORTED");
        assert_eq!(PrintError::Failed(String::new()).code(), "E_PRINT_FAILED");
    }
}
