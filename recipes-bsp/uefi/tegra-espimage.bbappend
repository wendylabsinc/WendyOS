# Add pseudo workaround for image tasks
# The wip-l4t-r38.4.0 branch has pseudo corruption issues with image tasks
python sstate_report_unihash() {
    report_unihash = getattr(bb.parse.siggen, 'report_unihash', None)

    if report_unihash:
        ss = sstate_state_fromvars(d)
        # Disable pseudo for all image-related tasks to avoid corruption
        if ss['task'] in ['image_complete', 'image_qa', 'image', 'flush_pseudodb']:
            os.environ['PSEUDO_DISABLED'] = '1'
        report_unihash(os.getcwd(), ss['task'], d)
}
