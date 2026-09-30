package tools

import "github.com/AIShie-Education/AIShie-Core/internal/apperr"

// A version of a document holds files (docs/schema.md §2.4, Files of a
// version), each a row of document_version_file, in order, and each file of
// material, instructions or a rubric has a text version of its own.

// errNoFile is what a file that does not exist, and one of a version the
// caller may not read, both answer.
var errNoFile = apperr.Missing("no such file of this document")
