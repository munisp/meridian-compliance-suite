"""Structural test harness for the meridian_nrs_einvoice Odoo addon.

Runs WITHOUT an Odoo runtime: validates everything that can be validated
statically so that regressions are caught in CI before live-Odoo UAT:

  1. __manifest__.py parses via ast.literal_eval, has the required keys, a
     17.0/18.0 version prefix, and every listed data file exists.
  2. security/ir.model.access.csv has the exact Odoo access-CSV header and
     well-formed rows.
  3. Every XML file is well-formed; every <record> id is unique; every
     inherit_id ref and xpath expr attribute is present and non-empty.
  4. Every addon .py file compiles (via compile()).
  5. The webhook controller's @http.route decorators carry the required
     auth/csrf/methods arguments (parsed from the AST, no Odoo import).
  6. The export-dict builder contract: account_move.py defines
     _nrs_export_dict, and three golden Odoo invoice dicts (as produced by
     that method) map through meridian_odoo_client.build_nrs_invoice to the
     expected NRS payloads — multi-tax-group, zero-rated, and a
     foreign-currency invoice rejected with a clear error.

Run:  cd integrations/odoo && python3 -m pytest tools -q
"""

import ast
import csv
import os
import sys
import unittest
import xml.etree.ElementTree as ET

ODOO_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ADDON_DIR = os.path.join(ODOO_DIR, "meridian_nrs_einvoice")
sys.path.insert(0, ODOO_DIR)

from meridian_odoo_client import NRSPayloadError, build_nrs_invoice  # noqa: E402

SERVICE_ID = "94ND90NR"
BUSINESS_ID = "biz-odoo-001"

MANIFEST_REQUIRED_KEYS = {
    "name", "version", "category", "summary", "description", "author",
    "license", "depends", "data", "installable", "application",
}

ACCESS_CSV_HEADER = [
    "id", "name", "model_id:id", "group_id:id",
    "perm_read", "perm_write", "perm_create", "perm_unlink",
]


def _load_manifest():
    path = os.path.join(ADDON_DIR, "__manifest__.py")
    with open(path, "r", encoding="utf-8") as fh:
        tree = ast.parse(fh.read(), filename=path)
    for node in tree.body:
        if isinstance(node, ast.Expr) and isinstance(node.value, ast.Dict):
            return ast.literal_eval(node.value)
    raise AssertionError("__manifest__.py contains no dict literal")


def _addon_files(suffix):
    out = []
    for root, _dirs, files in os.walk(ADDON_DIR):
        for f in sorted(files):
            if f.endswith(suffix):
                out.append(os.path.join(root, f))
    return sorted(out)


class TestManifest(unittest.TestCase):
    def test_manifest_parses_and_has_required_keys(self):
        manifest = _load_manifest()
        self.assertIsInstance(manifest, dict)
        missing = MANIFEST_REQUIRED_KEYS - set(manifest)
        self.assertFalse(missing, "manifest missing keys: %s" % sorted(missing))

    def test_manifest_version_prefix_17_or_18(self):
        # Odoo refuses to install a module whose version does not start
        # with the running series; both supported prefixes are accepted
        # here (see __manifest__.py header comment).
        version = _load_manifest()["version"]
        self.assertRegex(
            version, r"^(17|18)\.0\.\d+\.\d+\.\d+$",
            "version %r must start with 17.0. or 18.0." % version)

    def test_manifest_data_files_exist(self):
        manifest = _load_manifest()
        self.assertTrue(manifest["data"], "manifest lists no data files")
        for rel in manifest["data"]:
            path = os.path.join(ADDON_DIR, rel)
            self.assertTrue(os.path.isfile(path),
                            "manifest data file missing: %s" % rel)

    def test_manifest_17_18_compat_documented(self):
        with open(os.path.join(ADDON_DIR, "__manifest__.py"),
                  encoding="utf-8") as fh:
            text = fh.read()
        self.assertIn("17", text)
        self.assertIn("18", text)


class TestSecurityCSV(unittest.TestCase):
    def _rows(self):
        path = os.path.join(ADDON_DIR, "security", "ir.model.access.csv")
        with open(path, newline="", encoding="utf-8") as fh:
            return list(csv.reader(fh))

    def test_header_exact(self):
        self.assertEqual(self._rows()[0], ACCESS_CSV_HEADER)

    def test_rows_well_formed_and_unique_ids(self):
        rows = self._rows()
        ids = []
        for row in rows[1:]:
            self.assertEqual(len(row), len(ACCESS_CSV_HEADER),
                             "bad column count: %r" % row)
            self.assertTrue(all(cell.strip() for cell in row),
                            "empty cell in row: %r" % row)
            for perm in row[4:]:
                self.assertIn(perm, ("0", "1"),
                              "perm must be 0/1 in row: %r" % row)
            ids.append(row[0])
        self.assertEqual(len(ids), len(set(ids)), "duplicate access ids")


class TestXMLLint(unittest.TestCase):
    def test_xml_files_exist(self):
        self.assertTrue(_addon_files(".xml"), "no XML files in addon")

    def test_all_xml_well_formed(self):
        for path in _addon_files(".xml"):
            try:
                ET.parse(path)
            except ET.ParseError as exc:
                self.fail("%s not well-formed: %s" % (path, exc))

    def test_record_ids_unique_per_file(self):
        for path in _addon_files(".xml"):
            ids = [el.get("id")
                   for el in ET.parse(path).getroot().iter("record")]
            ids = [i for i in ids if i]
            self.assertEqual(len(ids), len(set(ids)),
                             "%s: duplicate record ids" % path)
            self.assertTrue(ids, "%s: no <record> elements" % path)

    def test_inherit_id_and_xpath_exprs_non_empty(self):
        for path in _addon_files(".xml"):
            root = ET.parse(path).getroot()
            for el in root.iter():
                # <field name="inherit_id" ref="..."/> must carry a ref
                if el.tag == "field" and el.get("name") == "inherit_id":
                    self.assertTrue((el.get("ref") or "").strip(),
                                    "%s: empty inherit_id ref" % path)
                for xp in el.iter("xpath"):
                    self.assertTrue((xp.get("expr") or "").strip(),
                                    "%s: empty xpath expr" % path)
                    self.assertTrue((xp.get("position") or "").strip(),
                                    "%s: xpath without position" % path)

    def test_no_legacy_cron_numbercall(self):
        # Odoo 17/18 hardening: the deprecated numbercall=-1 idiom must not
        # reappear; numbercall is omitted entirely.
        cron = os.path.join(ADDON_DIR, "data", "ir_cron.xml")
        root = ET.parse(cron).getroot()
        for field in root.iter("field"):
            self.assertNotEqual(field.get("name"), "numbercall",
                                "ir_cron.xml must omit numbercall (17/18)")


class TestPythonCompile(unittest.TestCase):
    def test_all_python_files_compile(self):
        files = _addon_files(".py")
        self.assertTrue(files, "no python files in addon")
        for path in files:
            with open(path, "r", encoding="utf-8") as fh:
                src = fh.read()
            try:
                compile(src, path, "exec")
            except SyntaxError as exc:
                self.fail("%s does not compile: %s" % (path, exc))


class TestControllerRoutes(unittest.TestCase):
    def _route_decorators(self):
        path = os.path.join(ADDON_DIR, "controllers", "webhook.py")
        with open(path, "r", encoding="utf-8") as fh:
            tree = ast.parse(fh.read(), filename=path)
        routes = []
        for node in ast.walk(tree):
            if not isinstance(node, ast.FunctionDef):
                continue
            for dec in node.decorator_list:
                if (isinstance(dec, ast.Call)
                        and isinstance(dec.func, ast.Attribute)
                        and dec.func.attr == "route"):
                    routes.append(dec)
        return routes

    def test_webhook_route_decorator_present(self):
        self.assertTrue(self._route_decorators(),
                        "no @http.route decorators in controllers/webhook.py")

    def test_webhook_route_arguments(self):
        for dec in self._route_decorators():
            kw = {k.arg: k.value for k in dec.keywords}
            for required in ("type", "auth", "methods", "csrf"):
                self.assertIn(required, kw,
                              "route missing %r argument" % required)
            self.assertEqual(ast.literal_eval(kw["auth"]), "none",
                             "webhook must be auth='none' (HMAC is the auth)")
            self.assertEqual(ast.literal_eval(kw["csrf"]), False,
                             "webhook must be csrf=False")
            self.assertEqual(ast.literal_eval(kw["methods"]), ["POST"],
                             "webhook must accept POST only")


# ---------------------------------------------------------------------------
# Golden export-dict payloads (as produced by account_move._nrs_export_dict)
# ---------------------------------------------------------------------------

SUPPLIER = {
    "name": "Lekki Medical Supplies Ltd",
    "tin": "12345678-0001",
    "email": "accounts@lekki-med.ng",
    "phone": "+2348012345678",
    "street": "14 Admiralty Way",
    "city": "Lekki",
    "state": "NG-LA",
    "country": "NG",
}
CUSTOMER = {
    "name": "Ikeja General Hospital",
    "tin": "87654321-0001",
    "city": "Ikeja",
    "state": "NG-LA",
    "country": "NG",
}


def golden_export_dict(**over):
    inv = {
        "invoice_number": "INV20260042",
        "issue_date": "2026-02-10",
        "due_date": "2026-03-12",
        "move_type": "out_invoice",
        "currency": "NGN",
        "supplier": SUPPLIER,
        "customer": CUSTOMER,
        "payment_means_code": "30",
        "lines": [],
    }
    inv.update(over)
    return inv


class TestExportDictGoldenPayloads(unittest.TestCase):
    def test_export_dict_builder_defined_in_addon(self):
        path = os.path.join(ADDON_DIR, "models", "account_move.py")
        with open(path, "r", encoding="utf-8") as fh:
            tree = ast.parse(fh.read(), filename=path)
        names = {n.name for n in ast.walk(tree)
                 if isinstance(n, ast.FunctionDef)}
        self.assertIn("_nrs_export_dict", names)

    def test_golden_multi_tax_group(self):
        # STANDARD_VAT + ZERO_VAT + EXEMPT lines -> 3 tax subtotals with
        # correct per-group kobo rounding.
        inv = golden_export_dict(lines=[
            {"name": "Surgical gloves (box)", "quantity": 2,
             "price_unit": 5000.00, "line_extension_amount": 10000.00,
             "tax_category": "STANDARD_VAT"},
            {"name": "Dialysis session", "quantity": 1,
             "price_unit": 20000.00, "line_extension_amount": 20000.00,
             "tax_category": "ZERO_VAT"},
            {"name": "Charity consultation", "quantity": 1,
             "price_unit": 3000.00, "line_extension_amount": 3000.00,
             "tax_category": "EXEMPT"},
        ])
        payload = build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertEqual(payload["irn"], "INV20260042-94ND90NR-20260210")
        self.assertEqual(payload["invoice_type_code"], "380")
        self.assertEqual(payload["document_currency_code"], "NGN")
        self.assertEqual(len(payload["invoice_line"]), 3)
        total = payload["legal_monetary_total"]
        self.assertEqual(total["tax_exclusive_amount"], 33000.00)
        # 7.5% of 10000 = 750; zero/exempt groups add nothing
        self.assertEqual(total["payable_amount"], 33750.00)
        subs = payload["tax_total"][0]["tax_subtotal"]
        self.assertEqual(len(subs), 3)
        by_cat = {s["tax_category"]["id"]: s for s in subs}
        self.assertEqual(by_cat["STANDARD_VAT"]["tax_amount"], 750.00)
        self.assertEqual(by_cat["ZERO_VAT"]["tax_amount"], 0.0)
        self.assertEqual(by_cat["ZERO_VAT"]["taxable_amount"], 20000.00)
        self.assertEqual(by_cat["EXEMPT"]["tax_amount"], 0.0)
        self.assertEqual(by_cat["STANDARD_VAT"]["tax_category"]["percent"],
                         7.5)

    def test_golden_zero_rated_medical(self):
        inv = golden_export_dict(lines=[
            {"name": "Tuition — term 1", "quantity": 1,
             "price_unit": 150000.005,  # x.xx5 half-up boundary
             "line_extension_amount": 150000.005,
             "tax_category": "ZERO_VAT"},
        ])
        payload = build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        line = payload["invoice_line"][0]
        # 150000.005 NGN -> 15000001 kobo (round-half-up in decimal space)
        self.assertEqual(line["line_extension_amount"], 150000.01)
        sub = payload["tax_total"][0]["tax_subtotal"][0]
        self.assertEqual(sub["tax_category"]["id"], "ZERO_VAT")
        self.assertEqual(sub["tax_amount"], 0.0)
        self.assertEqual(sub["taxable_amount"], 150000.01)
        self.assertEqual(payload["legal_monetary_total"]["payable_amount"],
                         150000.01)

    def test_golden_foreign_currency_rejected(self):
        inv = golden_export_dict(currency="USD", lines=[
            {"name": "Surgical gloves (box)", "quantity": 1,
             "price_unit": 10.00, "line_extension_amount": 10.00,
             "tax_category": "STANDARD_VAT"},
        ])
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        msg = str(ctx.exception)
        self.assertIn("USD", msg)
        self.assertIn("NGN", msg)
        self.assertIn("document_currency_code", msg)


if __name__ == "__main__":
    unittest.main()
