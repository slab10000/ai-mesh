#!/usr/bin/env python3
"""Generate the one-page ai-mesh remote PDF acceptance report."""
from pathlib import Path

OUTPUT = Path(__file__).with_name("remote-report.pdf")


def pdf_string(value: str) -> bytes:
    """Encode text as a safely escaped PDF literal string (Helvetica/WinAnsi)."""
    encoded = value.encode("cp1252")
    escaped = encoded.replace(b"\\", b"\\\\").replace(b"(", b"\\(").replace(b")", b"\\)")
    return b"(" + escaped + b")"


def make_pdf() -> bytes:
    # US Letter page with 72-point (1-inch) margins.
    commands = [
        b"BT",
        b"/F1 20 Tf",
        b"72 700 Td",
        pdf_string("ai-mesh remote PDF test") + b" Tj",
        b"/F1 12 Tf",
        b"0 -52 Td",
        pdf_string("Instructions originated on the Mac.") + b" Tj",
        b"0 -28 Td",
        pdf_string("PDF generated on green-lighthouse.") + b" Tj",
        b"0 -28 Td",
        pdf_string("Result returned to the Mac.") + b" Tj",
        b"0 -28 Td",
        pdf_string("Context marker: PDF_HANDOFF_42") + b" Tj",
        b"ET",
    ]
    stream = b"\n".join(commands) + b"\n"
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
        b"<< /Length " + str(len(stream)).encode("ascii") + b" >>\nstream\n" + stream + b"endstream",
    ]

    pdf = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = [0]
    for number, body in enumerate(objects, start=1):
        offsets.append(len(pdf))
        pdf.extend(f"{number} 0 obj\n".encode("ascii"))
        pdf.extend(body)
        pdf.extend(b"\nendobj\n")

    xref_offset = len(pdf)
    pdf.extend(f"xref\n0 {len(objects) + 1}\n".encode("ascii"))
    pdf.extend(b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        pdf.extend(f"{offset:010d} 00000 n \n".encode("ascii"))
    pdf.extend(
        f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\n"
        f"startxref\n{xref_offset}\n%%EOF\n".encode("ascii")
    )
    return bytes(pdf)


if __name__ == "__main__":
    OUTPUT.write_bytes(make_pdf())
    print(f"Wrote {OUTPUT}")
