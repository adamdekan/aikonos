// Importing this module registers every op handler as a side effect (each
// format module calls registerHandler at import time). Checkpoint 3 adds
// office.js (office.convert) and the xlsx.recalc/pptx.thumbnail/pdf.transform
// handlers registered inside the existing per-format modules.
import './docx.js';
import './xlsx.js';
import './pptx.js';
import './pdf.js';
import './office.js';
